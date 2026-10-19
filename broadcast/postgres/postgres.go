// Package postgres is the PostgreSQL adapter of package broadcast: it fans
// messages out across application replicas with LISTEN/NOTIFY, so a change
// made through one replica reaches browsers connected to another.
//
// Every broker listens on one notification channel ("forge_broadcast") of the
// database and delivers what it hears to its own in-process subscribers
// (package broadcast's Memory broker). Publishing sends a NOTIFY. A message
// published through a broker is delivered to its local subscribers directly,
// so a single replica works even while its listener reconnects.
//
// Limits and behavior worth knowing:
//
//   - NOTIFY payloads are limited to 8000 bytes. A message is therefore
//     split into chunks of 4500 bytes, sent as consecutive notifications of
//     one transaction and reassembled by the listener (see envelope.go);
//     messages are limited to Options.MaxMessageBytes (default 256 KiB, about
//     60 notifications). Send a smaller fragment, or have the client reload
//     the resource, for anything bigger.
//   - Delivery is at most once. Notifications sent while a listener is
//     disconnected are lost; the listener reconnects with backoff and logs
//     the gap. NOTIFY is not tenant-aware: it is a database-wide channel, and
//     the channel key (which contains the tenant) is what separates tenants,
//     exactly as for the in-memory broker. Whoever can NOTIFY on the database
//     can publish, so the database role is the trust boundary.
//   - The listener holds one connection from the pool for its whole life,
//     which counts against FORGE_DATABASE_MAX_CONNS. LISTEN does not work
//     through PgBouncer in transaction pooling mode; connect directly or use
//     session pooling.
//   - Memory is bounded: the Memory broker bounds subscribers and queues, and
//     reassembly buffers are capped in count, bytes and age.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rocketinsidecorp/forge-go/broadcast"
	forgepostgres "github.com/rocketinsidecorp/forge-go/postgres"
)

// DefaultNotifyChannel is the PostgreSQL notification channel brokers share.
const DefaultNotifyChannel = "forge_broadcast"

// DefaultMaxMessageBytes is the largest message a broker publishes.
const DefaultMaxMessageBytes = 256 << 10

const (
	maxPending      = 64
	pendingLifetime = 30 * time.Second
	minBackoff      = time.Second
	maxBackoff      = 30 * time.Second
)

var channelPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Options configures New.
type Options struct {
	// NotifyChannel is the PostgreSQL channel (a lowercase identifier of at
	// most 63 bytes). Every replica of an application must use the same one;
	// distinct applications sharing a database should use distinct ones.
	// Empty means DefaultNotifyChannel.
	NotifyChannel string
	// MaxMessageBytes limits one message. Zero means DefaultMaxMessageBytes.
	MaxMessageBytes int
	// Logger receives listener events. Nil means slog.Default.
	Logger *slog.Logger
}

// Broker is a broadcast.Broker backed by PostgreSQL LISTEN/NOTIFY.
type Broker struct {
	db       *forgepostgres.DB
	local    *broadcast.Memory
	channel  string
	origin   string
	maxBytes int
	logger   *slog.Logger

	cancel    context.CancelFunc
	done      chan struct{}
	stopOnce  sync.Once
	listening atomic.Bool
	now       func() time.Time
}

var _ broadcast.Broker = (*Broker)(nil)

// New starts a broker whose listener runs until Shutdown or ctx is cancelled.
// local holds the in-process subscribers; build it with broadcast.NewMemory
// (the broker closes it on Shutdown). The goroutine is owned by the broker:
// register Shutdown with App.OnShutdown.
func New(ctx context.Context, db *forgepostgres.DB, local *broadcast.Memory, opts Options) (*Broker, error) {
	if ctx == nil || db == nil || local == nil {
		return nil, errors.New("broadcast/postgres: a context, a database and a local broker are required")
	}
	if opts.NotifyChannel == "" {
		opts.NotifyChannel = DefaultNotifyChannel
	}
	if !channelPattern.MatchString(opts.NotifyChannel) {
		return nil, errors.New("broadcast/postgres: the notify channel must be a lowercase identifier of at most 63 bytes")
	}
	if opts.MaxMessageBytes < 0 {
		return nil, errors.New("broadcast/postgres: the message limit cannot be negative")
	}
	if opts.MaxMessageBytes == 0 {
		opts.MaxMessageBytes = DefaultMaxMessageBytes
	}
	opts.MaxMessageBytes = min(opts.MaxMessageBytes, local.MaxMessageBytes())
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	origin, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	// #nosec G118 -- cancel is stored in the broker and called by Shutdown.
	runCtx, cancel := context.WithCancel(ctx)
	broker := &Broker{
		db: db, local: local, channel: opts.NotifyChannel, origin: origin,
		maxBytes: opts.MaxMessageBytes, logger: opts.Logger,
		cancel: cancel, done: make(chan struct{}), now: time.Now,
	}
	go broker.run(runCtx)
	return broker, nil
}

// Listening reports whether the listener currently holds its LISTEN
// connection, for readiness checks and tests.
func (b *Broker) Listening() bool { return b.listening.Load() }

// Subscribers is the number of local subscriptions.
func (b *Broker) Subscribers() int { return b.local.Subscribers() }

// Subscribe registers a local subscriber; see broadcast.Memory.Subscribe.
func (b *Broker) Subscribe(ctx context.Context, channel broadcast.Channel) (broadcast.Subscription, error) {
	return b.local.Subscribe(ctx, channel)
}

// Publish delivers message to this replica's subscribers immediately and to
// every other replica through NOTIFY. If the notification fails the error is
// returned although local subscribers already have the message.
func (b *Broker) Publish(ctx context.Context, channel broadcast.Channel, message broadcast.Message) error {
	if len(message.Data) > b.maxBytes {
		return broadcast.ErrMessageTooLarge
	}
	if err := b.local.Publish(ctx, channel, message); err != nil {
		return err
	}
	chunks, err := b.chunks(b.origin, channel, message)
	if err != nil {
		return err
	}
	return b.db.InTx(ctx, func(tx pgx.Tx) error { return b.notify(ctx, tx, chunks) })
}

// PublishTx queues message in the caller's transaction: it is delivered, to
// every replica including this one, only if the transaction commits, and
// never if it rolls back. Use it to broadcast a change atomically with the
// write that caused it.
func (b *Broker) PublishTx(ctx context.Context, tx pgx.Tx, channel broadcast.Channel, message broadcast.Message) error {
	if len(message.Data) > b.maxBytes {
		return broadcast.ErrMessageTooLarge
	}
	chunks, err := b.chunks("-", channel, message)
	if err != nil {
		return err
	}
	return b.notify(ctx, tx, chunks)
}

func (b *Broker) chunks(origin string, channel broadcast.Channel, message broadcast.Message) ([]chunk, error) {
	if _, err := broadcast.ParseKey(channel.Key()); err != nil {
		return nil, err
	}
	id, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	return split(origin, id, channel.Key(), []byte(message.Data)), nil
}

func (b *Broker) notify(ctx context.Context, tx pgx.Tx, chunks []chunk) error {
	for _, c := range chunks {
		payload := c.encode()
		if len(payload) > maxNotifyBytes {
			return fmt.Errorf("broadcast/postgres: notification of %d bytes exceeds the limit", len(payload))
		}
		if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", b.channel, payload); err != nil {
			return fmt.Errorf("broadcast/postgres: notify: %w", err)
		}
	}
	return nil
}

// Shutdown stops the listener, waits for it (until ctx ends) and closes the
// local broker, which ends every open event stream. It is a
// forge.ShutdownHook.
func (b *Broker) Shutdown(ctx context.Context) error {
	b.stopOnce.Do(b.cancel)
	b.local.Close()
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("broadcast/postgres: stop listener: %w", ctx.Err())
	}
}

func (b *Broker) run(ctx context.Context) {
	defer close(b.done)
	defer b.listening.Store(false)
	backoff := minBackoff
	for ctx.Err() == nil {
		connected, err := b.listen(ctx)
		b.listening.Store(false)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = minBackoff
		}
		b.logger.Error("broadcast listener disconnected; messages sent meanwhile are lost", "error", err, "retry_in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// listen holds one connection LISTENing until it fails or ctx ends. It
// reports whether LISTEN succeeded.
func (b *Broker) listen(ctx context.Context) (connected bool, err error) {
	pooled, err := b.db.Pool().Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	// Hijack so the connection leaves the pool and is closed, rather than
	// returning to it still LISTENing.
	defer closeHijacked(pooled)
	conn := pooled.Conn()
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{b.channel}.Sanitize()); err != nil {
		return false, fmt.Errorf("listen: %w", err)
	}
	b.listening.Store(true)
	assembly := newAssembler(maxPending, b.maxBytes, pendingLifetime, b.now)
	maxChunks := (b.maxBytes + chunkBytes - 1) / chunkBytes
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, fmt.Errorf("wait for notification: %w", err)
		}
		b.deliver(ctx, assembly, notification.Payload, max(1, maxChunks))
	}
}

func closeHijacked(conn *pgxpool.Conn) {
	raw := conn.Hijack()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = raw.Close(ctx)
}

// deliver handles one notification. Anything malformed is logged and
// dropped: the channel is writable by anyone with database access, and a bad
// payload must never stop the listener.
func (b *Broker) deliver(ctx context.Context, assembly *assembler, payload string, maxChunks int) {
	received, err := parseChunk(payload, maxChunks)
	if err != nil {
		b.logger.Warn("ignored a malformed broadcast notification", "error", err)
		return
	}
	if received.origin == b.origin {
		return // already delivered locally by Publish
	}
	key, data, _, done, err := assembly.add(received)
	if err != nil {
		b.logger.Warn("dropped a broadcast message", "error", err)
		return
	}
	if !done {
		return
	}
	channel, err := broadcast.ParseKey(key)
	if err != nil {
		b.logger.Warn("ignored a broadcast notification for an invalid channel")
		return
	}
	if err := b.local.Publish(ctx, channel, broadcast.Message{Data: string(data)}); err != nil && ctx.Err() == nil {
		b.logger.Warn("could not deliver a broadcast message", "error", err)
	}
}

func randomHex(n int) (string, error) {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("broadcast/postgres: random id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}
