package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fabriciobonjorno/forge-go/auth"
	"github.com/fabriciobonjorno/forge-go/tenancy"
	"github.com/fabriciobonjorno/forge-go/uuid"
)

// Defaults applied to zero Options fields.
const (
	DefaultQueueSize               = 256
	DefaultMaxPayloadBytes         = 16 << 10
	DefaultMaxConns                = 10_000
	DefaultMaxConnsPerSubject      = 32
	DefaultMaxSubscriptionsPerConn = 64
	DefaultMaxSubscriptions        = 100_000
	DefaultReplayEvents            = 1024
	DefaultReplayBytes             = 4 << 20
	DefaultReplayTTL               = 5 * time.Minute
	DefaultMaxConnLifetime         = time.Hour
	DefaultRevalidateInterval      = time.Minute
)

// Options configures a Broker. Zero values select the defaults above; fields
// documented as such use a negative value to disable the feature.
type Options struct {
	Logger *slog.Logger
	// Channels are registered at construction; more can be added with
	// Register.
	Channels []Channel
	// Transport forwards events to, and receives them from, other replicas.
	// Nil keeps the broker in-process only.
	Transport Transport

	// QueueSize is the per-connection buffer. A connection that lets it fill
	// is disconnected with ErrSlowConsumer.
	QueueSize int
	// MaxPayloadBytes bounds an event's JSON payload. With a Transport it is
	// lowered to Transport.MaxPayloadBytes so every replica accepts the same
	// events.
	MaxPayloadBytes int
	// MaxConns bounds open connections; MaxConnsPerSubject bounds them per
	// authenticated subject.
	MaxConns           int
	MaxConnsPerSubject int
	// MaxSubscriptionsPerConn and MaxSubscriptions bound topic subscriptions
	// per connection and in total.
	MaxSubscriptionsPerConn int
	MaxSubscriptions        int

	// ReplayEvents, ReplayBytes and ReplayTTL bound the in-memory window used
	// for Last-Event-ID resume. A negative ReplayEvents disables replay: every
	// resume then reports a gap.
	ReplayEvents int
	ReplayBytes  int
	ReplayTTL    time.Duration

	// MaxConnLifetime closes a connection after roughly this long (with up to
	// 10% jitter) so clients re-authenticate; browsers reconnect by
	// themselves. Negative disables the limit.
	MaxConnLifetime time.Duration
	// Revalidate, when set, runs every RevalidateInterval for each open
	// connection; an error closes it with ErrSessionExpired. Use it to
	// notice revoked sessions and changed permissions sooner than
	// MaxConnLifetime.
	Revalidate         func(ctx context.Context, principal auth.Principal) error
	RevalidateInterval time.Duration

	// Clock replaces time.Now in tests.
	Clock func() time.Time
}

func (o *Options) applyDefaults() error {
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	for _, field := range []struct {
		value *int
		def   int
		name  string
	}{
		{&o.QueueSize, DefaultQueueSize, "QueueSize"},
		{&o.MaxPayloadBytes, DefaultMaxPayloadBytes, "MaxPayloadBytes"},
		{&o.MaxConns, DefaultMaxConns, "MaxConns"},
		{&o.MaxConnsPerSubject, DefaultMaxConnsPerSubject, "MaxConnsPerSubject"},
		{&o.MaxSubscriptionsPerConn, DefaultMaxSubscriptionsPerConn, "MaxSubscriptionsPerConn"},
		{&o.MaxSubscriptions, DefaultMaxSubscriptions, "MaxSubscriptions"},
		{&o.ReplayBytes, DefaultReplayBytes, "ReplayBytes"},
	} {
		switch {
		case *field.value == 0:
			*field.value = field.def
		case *field.value < 0:
			return fmt.Errorf("realtime: %s must be positive", field.name)
		}
	}
	if o.ReplayEvents == 0 {
		o.ReplayEvents = DefaultReplayEvents
	}
	if o.ReplayTTL == 0 {
		o.ReplayTTL = DefaultReplayTTL
	}
	if o.MaxConnLifetime == 0 {
		o.MaxConnLifetime = DefaultMaxConnLifetime
	}
	if o.RevalidateInterval == 0 {
		o.RevalidateInterval = DefaultRevalidateInterval
	}
	if o.ReplayTTL < 0 || o.RevalidateInterval < 0 {
		return errors.New("realtime: ReplayTTL and RevalidateInterval must be positive")
	}
	if o.QueueSize < 4 {
		return errors.New("realtime: QueueSize must be at least 4")
	}
	if o.Transport != nil {
		o.MaxPayloadBytes = min(o.MaxPayloadBytes, o.Transport.MaxPayloadBytes())
		if o.MaxPayloadBytes <= 0 {
			return errors.New("realtime: transport allows no payload")
		}
	}
	return nil
}

// Stats is a point-in-time snapshot of broker activity.
type Stats struct {
	Conns         int
	Subscriptions int
	ReplayEvents  int

	Published       uint64 // events published on this replica
	Received        uint64 // events accepted from other replicas
	Rejected        uint64 // remote envelopes dropped as invalid
	Delivered       uint64 // events queued to connections
	SlowDisconnects uint64
	Gaps            uint64 // times the transport reported lost events
}

// Broker routes published events to subscribed connections. It is safe for
// concurrent use. Publishing never blocks on a subscriber.
type Broker struct {
	id        uuid.UUID
	opts      Options
	logger    *slog.Logger
	transport Transport
	now       func() time.Time

	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	chMu     sync.RWMutex
	channels []*channel

	mu            sync.Mutex
	closed        bool
	topics        map[string]map[*Conn]struct{}
	conns         map[*Conn]struct{}
	perSubject    map[uuid.UUID]int
	subscriptions int
	replay        *replayStore

	published, received, rejected, delivered, slow, gaps atomic.Uint64
}

// New creates a broker. It closes by itself when ctx is cancelled, which lets
// the same signal context that stops the HTTP server also end open streams;
// otherwise a long-lived stream would hold up http.Server.Shutdown until the
// drain timeout. Close it explicitly (or register Close with App.OnShutdown)
// to wait for the transport to stop.
func New(ctx context.Context, opts Options) (*Broker, error) {
	if ctx == nil {
		return nil, errNilContext
	}
	if err := opts.applyDefaults(); err != nil {
		return nil, err
	}
	id, err := uuid.New()
	if err != nil {
		return nil, err
	}
	started := opts.Clock()
	broker := &Broker{
		id:         id,
		opts:       opts,
		logger:     opts.Logger,
		transport:  opts.Transport,
		now:        opts.Clock,
		topics:     map[string]map[*Conn]struct{}{},
		conns:      map[*Conn]struct{}{},
		perSubject: map[uuid.UUID]int{},
		// Events published before this process started are not in the window,
		// so a client resuming from one of them must be told to resync.
		replay: newReplayStore(opts.ReplayEvents, opts.ReplayBytes, opts.ReplayTTL, floorAt(started)),
	}
	broker.runCtx, broker.cancel = context.WithCancel(ctx)
	if err := broker.Register(opts.Channels...); err != nil {
		broker.cancel()
		return nil, err
	}
	context.AfterFunc(broker.runCtx, broker.closeConns)
	if broker.transport != nil {
		broker.wg.Go(func() {
			err := broker.transport.Run(broker.runCtx, remote{broker})
			if err != nil && broker.runCtx.Err() == nil {
				broker.logger.Error("realtime transport stopped; events are no longer shared between replicas", "error", err)
			}
		})
	}
	return broker, nil
}

// Close stops accepting connections, disconnects every open one with
// ErrClosed, stops the transport and waits for it until ctx ends. It is safe
// to call more than once. Its signature matches forge.ShutdownHook.
func (b *Broker) Close(ctx context.Context) error {
	b.cancel()
	b.closeConns()
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("realtime: waiting for transport to stop: %w", ctx.Err())
	}
}

func (b *Broker) closeConns() {
	b.mu.Lock()
	b.closed = true
	open := make([]*Conn, 0, len(b.conns))
	for c := range b.conns {
		open = append(open, c)
	}
	b.mu.Unlock()
	for _, c := range open {
		c.closeWith(ErrClosed)
	}
}

// Stats returns a snapshot of counters and gauges.
func (b *Broker) Stats() Stats {
	b.mu.Lock()
	conns, subscriptions, retained := len(b.conns), b.subscriptions, b.replay.len()
	b.mu.Unlock()
	return Stats{
		Conns: conns, Subscriptions: subscriptions, ReplayEvents: retained,
		Published: b.published.Load(), Received: b.received.Load(), Rejected: b.rejected.Load(),
		Delivered: b.delivered.Load(), SlowDisconnects: b.slow.Load(), Gaps: b.gaps.Load(),
	}
}

type publishConfig struct {
	global    bool
	tenant    *tenancy.Tenant
	ephemeral bool
}

// PublishOption customizes Publish.
type PublishOption func(*publishConfig)

// Global publishes to the topic shared by all tenants (subscribers reach it
// only through a Channel with Global set). Without it Publish uses the
// tenant in the context.
func Global() PublishOption { return func(c *publishConfig) { c.global = true } }

// ForTenant publishes to an explicit tenant instead of the one in the
// context, for code that runs outside a request such as a job.
func ForTenant(tenant tenancy.Tenant) PublishOption {
	return func(c *publishConfig) { c.tenant = &tenant }
}

// Ephemeral skips the replay window: the event reaches current subscribers
// only. Use it for transient signals such as typing indicators.
func Ephemeral() PublishOption { return func(c *publishConfig) { c.ephemeral = true } }

// Publish sends payload, encoded as JSON, to the topic's subscribers in the
// context's tenant (see Global and ForTenant) and, through the Transport, to
// other replicas. Subscribers on this replica are served even when the
// transport fails; the returned error then satisfies errors.Is(err,
// ErrTransport) and the returned Event is valid.
func (b *Broker) Publish(ctx context.Context, topic, eventType string, payload any, options ...PublishOption) (Event, error) {
	if ctx == nil {
		return Event{}, errNilContext
	}
	if !topicPattern.MatchString(topic) {
		return Event{}, ErrTopicInvalid
	}
	if !eventTypePattern.MatchString(eventType) || strings.HasPrefix(eventType, SyntheticPrefix) {
		return Event{}, ErrEventTypeInvalid
	}
	var cfg publishConfig
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	var target scope
	switch {
	case cfg.global && cfg.tenant != nil:
		return Event{}, errors.New("realtime: Global and ForTenant are mutually exclusive")
	case cfg.global:
		target = scope{global: true}
	case cfg.tenant != nil:
		tenant, err := tenancy.New(cfg.tenant.ID)
		if err != nil {
			return Event{}, err
		}
		target = tenantScope(tenant)
	default:
		tenant, err := tenancy.Require(ctx)
		if err != nil {
			return Event{}, err
		}
		target = tenantScope(tenant)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, ErrPayloadInvalid.WithCause(err)
	}
	if len(data) > b.opts.MaxPayloadBytes {
		return Event{}, ErrPayloadTooLarge
	}
	id, err := uuid.New()
	if err != nil {
		return Event{}, fmt.Errorf("realtime: generate event ID: %w", err)
	}
	event := Event{ID: id, Topic: topic, Type: eventType, Time: b.now().UTC(), Data: data}
	if !b.dispatch(target, event, !cfg.ephemeral) {
		return Event{}, ErrClosed
	}
	b.published.Add(1)
	if b.transport != nil {
		envelope := Envelope{Origin: b.id, Scope: target.String(), Ephemeral: cfg.ephemeral, Event: event}
		if err := b.transport.Publish(ctx, envelope); err != nil {
			b.logger.Warn("realtime event not forwarded to other replicas", "topic", topic, "error", err)
			return event, ErrTransport.WithCause(err)
		}
	}
	return event, nil
}

// dispatch stores the event for replay and queues it on every subscribed
// connection without blocking. Connections whose queue is full are closed
// after the lock is released. It reports false when the broker is closed.
func (b *Broker) dispatch(target scope, event Event, store bool) bool {
	key := target.key(event.Topic)
	var slowConns []*Conn
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false
	}
	if store {
		b.replay.add(key, event, b.now())
	}
	delivered := uint64(0)
	for c := range b.topics[key] {
		if c.enqueue(event) {
			delivered++
		} else {
			slowConns = append(slowConns, c)
		}
	}
	b.mu.Unlock()
	b.delivered.Add(delivered)
	for _, c := range slowConns {
		c.closeWith(ErrSlowConsumer)
	}
	return true
}

// remote adapts the broker to the Receiver a Transport delivers into.
type remote struct{ b *Broker }

// Deliver validates an envelope from another replica, which is untrusted
// input, and routes it. Envelopes this broker published itself are ignored.
func (r remote) Deliver(envelope Envelope) error {
	b := r.b
	if envelope.Origin == b.id {
		return nil
	}
	target, err := parseScope(envelope.Scope)
	event := envelope.Event
	switch {
	case err != nil:
	case !topicPattern.MatchString(event.Topic):
		err = ErrTopicInvalid
	case !eventTypePattern.MatchString(event.Type) || event.Synthetic():
		err = ErrEventTypeInvalid
	case event.ID.Version() != 7 || event.ID.Variant() != 2:
		err = errors.New("event ID must be a UUIDv7")
	case len(event.Data) > b.opts.MaxPayloadBytes:
		err = ErrPayloadTooLarge
	case !json.Valid(event.Data):
		err = ErrPayloadInvalid
	}
	if err != nil {
		b.rejected.Add(1)
		b.logger.Warn("realtime envelope rejected", "error", err)
		return err
	}
	b.received.Add(1)
	b.dispatch(target, event, !envelope.Ephemeral)
	return nil
}

// Gap reports that events from other replicas may have been lost, such as
// after a dropped database connection. Every connection receives a resync for
// each of its topics, and later resumes are told about the gap.
func (r remote) Gap() {
	b := r.b
	b.gaps.Add(1)
	var slowConns []*Conn
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.replay.markGap(ceilingAt(b.now()))
	for c := range b.conns {
		if !c.enqueueResync() {
			slowConns = append(slowConns, c)
		}
	}
	b.mu.Unlock()
	for _, c := range slowConns {
		c.closeWith(ErrSlowConsumer)
	}
}
