package realtime

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fabriciobonjorno/forge-go/auth"
	"github.com/fabriciobonjorno/forge-go/tenancy"
	"github.com/fabriciobonjorno/forge-go/uuid"
)

// Conn is one authenticated client connection (an SSE stream or a WebSocket)
// with a bounded queue of events. The transport adapter drains Events and
// stops when Done is closed. A Conn is bound to the principal and tenant in
// the context given to NewConn for its whole life.
type Conn struct {
	broker    *Broker
	principal auth.Principal
	tenant    tenancy.Tenant
	queue     chan Event
	done      chan struct{}

	closed atomic.Bool
	once   sync.Once
	err    error // written before done is closed

	mu   sync.Mutex
	subs map[string]string // logical topic -> routing key

	stops []func() bool
	reval atomic.Pointer[time.Timer]
}

// NewConn registers a connection for the principal and tenant in ctx, which
// must come from the authentication middleware. The connection closes when ctx
// ends, when the broker closes, after MaxConnLifetime, if Revalidate fails, or
// when its queue overflows. Always call Close when finished.
func (b *Broker) NewConn(ctx context.Context) (*Conn, error) {
	if ctx == nil {
		return nil, errNilContext
	}
	principal, ok := auth.FromContext(ctx)
	if !ok {
		return nil, ErrUnauthenticated
	}
	tenant, err := tenancy.Require(ctx)
	if err != nil || tenant.ID != principal.Tenant().ID {
		return nil, ErrUnauthenticated
	}
	c := &Conn{
		broker:    b,
		principal: principal,
		tenant:    tenant,
		queue:     make(chan Event, b.opts.QueueSize),
		done:      make(chan struct{}),
		subs:      map[string]string{},
	}
	subject := principal.SubjectID()
	b.mu.Lock()
	switch {
	case b.closed:
		b.mu.Unlock()
		return nil, ErrClosed
	case len(b.conns) >= b.opts.MaxConns, b.perSubject[subject] >= b.opts.MaxConnsPerSubject:
		b.mu.Unlock()
		return nil, ErrLimitReached
	}
	b.conns[c] = struct{}{}
	b.perSubject[subject]++
	b.mu.Unlock()

	c.addStop(context.AfterFunc(ctx, func() { c.closeWith(ctx.Err()) }))
	if lifetime := b.opts.MaxConnLifetime; lifetime > 0 {
		c.addStop(time.AfterFunc(jitter(lifetime), func() { c.closeWith(ErrSessionExpired) }).Stop)
	}
	if b.opts.Revalidate != nil {
		c.scheduleRevalidate(ctx)
	}
	return c, nil
}

// addStop registers a cleanup for closeWith. If the connection is already
// closed the cleanup runs at once.
func (c *Conn) addStop(stop func() bool) {
	c.mu.Lock()
	if !c.closed.Load() {
		c.stops = append(c.stops, stop)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	stop()
}

func (c *Conn) scheduleRevalidate(ctx context.Context) {
	interval := c.broker.opts.RevalidateInterval
	timer := time.AfterFunc(jitter(interval), func() {
		check, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.broker.opts.Revalidate(check, c.principal)
		cancel()
		if err != nil {
			c.broker.logger.Info("realtime connection revalidation failed", "subject", c.principal.SubjectID().String(), "error", err)
			c.closeWith(ErrSessionExpired)
			return
		}
		c.scheduleRevalidate(ctx)
	})
	c.reval.Store(timer)
	if c.closed.Load() {
		timer.Stop()
	}
}

// jitter spreads d by up to +10% so connections opened together do not
// reconnect together.
func jitter(d time.Duration) time.Duration {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return d
	}
	return d + time.Duration(float64(d)*0.1*float64(binary.BigEndian.Uint32(raw[:]))/float64(1<<32))
}

// Principal returns the authenticated principal the connection acts for.
func (c *Conn) Principal() auth.Principal { return c.principal }

// Events delivers queued events. It is never closed; select on Done as well.
func (c *Conn) Events() <-chan Event { return c.queue }

// Done is closed when the connection is closed, for whatever reason.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err explains why the connection closed: ErrSlowConsumer, ErrSessionExpired,
// ErrClosed, the context's error, or nil for a plain Close or while open.
func (c *Conn) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// Close releases the connection and its subscriptions. It is idempotent.
func (c *Conn) Close() { c.closeWith(nil) }

func (c *Conn) closeWith(reason error) {
	c.once.Do(func() {
		c.closed.Store(true)
		c.err = reason
		if errors.Is(reason, ErrSlowConsumer) {
			c.broker.slow.Add(1)
			c.broker.logger.Warn("realtime connection closed: consumer too slow",
				"subject", c.principal.SubjectID().String(), "queue", cap(c.queue))
		}
		c.broker.remove(c)
		c.mu.Lock()
		stops := c.stops
		c.mu.Unlock()
		for _, stop := range stops {
			stop()
		}
		if timer := c.reval.Load(); timer != nil {
			timer.Stop()
		}
		close(c.done)
	})
}

// enqueue adds event without blocking. A closed connection silently accepts
// it (the event is simply not delivered); false means the queue is full.
func (c *Conn) enqueue(event Event) bool {
	if c.closed.Load() {
		return true
	}
	select {
	case c.queue <- event:
		return true
	default:
		return false
	}
}

// enqueueResync queues one resync per subscribed topic. The broker mutex is
// held by callers, so c.subs cannot change concurrently with registration.
func (c *Conn) enqueueResync() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for topic := range c.subs {
		if !c.enqueue(Event{Topic: topic, Type: EventResync}) {
			return false
		}
	}
	return true
}

// SubscribeOption customizes Subscribe.
type SubscribeOption func(*subscribeConfig)

type subscribeConfig struct{ after uuid.UUID }

// After asks for retained events newer than id (the client's Last-Event-ID).
// If the retained window may not cover the gap, a resync event is queued
// first.
func After(id uuid.UUID) SubscribeOption { return func(c *subscribeConfig) { c.after = id } }

// Subscribe authorizes and registers a topic subscription. ctx should derive
// from the request and is passed to Channel.Authorize. On success an
// EventSubscribed acknowledgement is queued, then any replayed events, before
// live events. Subscribing again to a topic already subscribed is a no-op.
func (c *Conn) Subscribe(ctx context.Context, topic string, options ...SubscribeOption) error {
	if ctx == nil {
		return errNilContext
	}
	if !topicPattern.MatchString(topic) {
		return ErrTopicInvalid
	}
	var cfg subscribeConfig
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	target, err := c.authorizeSubscription(ctx, topic)
	if err != nil {
		return err
	}
	key := target.key(topic)

	b := c.broker
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	if c.closed.Load() {
		b.mu.Unlock()
		if reason := c.Err(); reason != nil {
			return reason
		}
		return ErrClosed
	}
	c.mu.Lock()
	if _, exists := c.subs[topic]; exists {
		c.mu.Unlock()
		b.mu.Unlock()
		return nil
	}
	if len(c.subs) >= b.opts.MaxSubscriptionsPerConn || b.subscriptions >= b.opts.MaxSubscriptions {
		c.mu.Unlock()
		b.mu.Unlock()
		return ErrLimitReached
	}
	c.subs[topic] = key
	c.mu.Unlock()
	members := b.topics[key]
	if members == nil {
		members = map[*Conn]struct{}{}
		b.topics[key] = members
	}
	members[c] = struct{}{}
	b.subscriptions++

	// The acknowledgement and any replay are queued under the broker lock,
	// which publishing also holds, so replay and live delivery neither overlap
	// nor leave a gap.
	ok := c.enqueue(Event{Topic: topic, Type: EventSubscribed})
	if ok && cfg.after != (uuid.UUID{}) {
		now := b.now()
		events, gap := b.replay.since(key, cfg.after, now, b.opts.QueueSize/2)
		if gap {
			ok = c.enqueue(Event{Topic: topic, Type: EventResync})
		}
		for _, event := range events {
			if !ok {
				break
			}
			ok = c.enqueue(event)
		}
	}
	b.mu.Unlock()
	if !ok {
		c.closeWith(ErrSlowConsumer)
		return ErrSlowConsumer
	}
	return nil
}

// Unsubscribe removes a topic subscription. Unknown topics are ignored.
func (c *Conn) Unsubscribe(topic string) {
	b := c.broker
	b.mu.Lock()
	defer b.mu.Unlock()
	c.mu.Lock()
	key, ok := c.subs[topic]
	delete(c.subs, topic)
	c.mu.Unlock()
	if ok {
		b.unsubscribeLocked(c, key)
	}
}

// Topics returns the logical topics the connection is subscribed to.
func (c *Conn) Topics() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	topics := make([]string, 0, len(c.subs))
	for topic := range c.subs {
		topics = append(topics, topic)
	}
	return topics
}

// unsubscribeLocked drops c from key's members. b.mu must be held.
func (b *Broker) unsubscribeLocked(c *Conn, key string) {
	members := b.topics[key]
	if _, ok := members[c]; !ok {
		return
	}
	delete(members, c)
	if len(members) == 0 {
		delete(b.topics, key)
	}
	b.subscriptions--
}

// remove forgets a closed connection and all of its subscriptions.
func (b *Broker) remove(c *Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.conns[c]; !ok {
		return
	}
	delete(b.conns, c)
	subject := c.principal.SubjectID()
	if b.perSubject[subject]--; b.perSubject[subject] <= 0 {
		delete(b.perSubject, subject)
	}
	c.mu.Lock()
	keys := make([]string, 0, len(c.subs))
	for _, key := range c.subs {
		keys = append(keys, key)
	}
	clear(c.subs)
	c.mu.Unlock()
	for _, key := range keys {
		b.unsubscribeLocked(c, key)
	}
}
