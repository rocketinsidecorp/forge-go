package broadcast

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Default limits of the in-memory broker.
const (
	DefaultMaxSubscribers           = 10000
	DefaultMaxSubscribersPerChannel = 1000
	DefaultBuffer                   = 64
	DefaultMaxMessageBytes          = 1 << 20
)

// MemoryOptions bound the in-memory broker. Zero values mean the defaults.
type MemoryOptions struct {
	// MaxSubscribers limits live subscriptions in the process.
	MaxSubscribers int
	// MaxSubscribersPerChannel limits live subscriptions to one channel.
	MaxSubscribersPerChannel int
	// Buffer is how many undelivered messages a subscriber may have queued.
	// A subscriber whose queue is full when a message arrives is dropped.
	Buffer int
	// MaxMessageBytes limits the size of one message.
	MaxMessageBytes int
}

// Memory is a Broker for one process. It starts no goroutines: Publish does
// a non-blocking send to each subscriber's bounded queue, and a subscriber
// that cannot keep up is dropped rather than slowing the publisher or
// growing memory. It does not reach other replicas; use broadcast/postgres
// for that.
type Memory struct {
	opts MemoryOptions

	mu       sync.Mutex
	closed   bool
	total    int
	channels map[string]map[*subscription]struct{}
	dropped  atomic.Int64
	stop     func() bool
}

// NewMemory returns a broker that closes itself, ending every subscription
// with ErrClosed, when ctx is cancelled. Pass the context Configure receives:
// it is cancelled when shutdown begins, which lets open event streams end so
// the HTTP server can drain. Also register Shutdown with App.OnShutdown.
func NewMemory(ctx context.Context, opts MemoryOptions) (*Memory, error) {
	if ctx == nil {
		return nil, errors.New("broadcast: a context is required")
	}
	if opts.MaxSubscribers < 0 || opts.MaxSubscribersPerChannel < 0 || opts.Buffer < 0 || opts.MaxMessageBytes < 0 {
		return nil, errors.New("broadcast: memory broker limits cannot be negative")
	}
	if opts.MaxSubscribers == 0 {
		opts.MaxSubscribers = DefaultMaxSubscribers
	}
	if opts.MaxSubscribersPerChannel == 0 {
		opts.MaxSubscribersPerChannel = DefaultMaxSubscribersPerChannel
	}
	if opts.Buffer == 0 {
		opts.Buffer = DefaultBuffer
	}
	if opts.MaxMessageBytes == 0 {
		opts.MaxMessageBytes = DefaultMaxMessageBytes
	}
	memory := &Memory{opts: opts, channels: map[string]map[*subscription]struct{}{}}
	memory.stop = context.AfterFunc(ctx, memory.Close)
	return memory, nil
}

// Close ends every subscription with ErrClosed and refuses new ones. It is
// idempotent and safe to call concurrently.
func (m *Memory) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	m.stop()
	for key, subscribers := range m.channels {
		for sub := range subscribers {
			sub.end(ErrClosed)
		}
		delete(m.channels, key)
	}
	m.total = 0
}

// Shutdown is Close as a shutdown hook (forge.ShutdownHook).
func (m *Memory) Shutdown(context.Context) error {
	m.Close()
	return nil
}

// Subscribers is the number of live subscriptions.
func (m *Memory) Subscribers() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

// Dropped is how many subscriptions were ended because their queue was full.
func (m *Memory) Dropped() int64 { return m.dropped.Load() }

// MaxMessageBytes is the largest message Publish accepts.
func (m *Memory) MaxMessageBytes() int { return m.opts.MaxMessageBytes }

// Publish delivers message to the subscribers of channel without waiting.
func (m *Memory) Publish(ctx context.Context, channel Channel, message Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := channel.valid(); err != nil {
		return err
	}
	if len(message.Data) > m.opts.MaxMessageBytes {
		return ErrMessageTooLarge
	}
	key := channel.Key()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	for sub := range m.channels[key] {
		select {
		case sub.messages <- message:
		default:
			m.removeLocked(sub)
			sub.end(ErrSlowConsumer)
			m.dropped.Add(1)
		}
	}
	return nil
}

// Subscribe registers a subscriber with a bounded queue. It fails with
// ErrTooManySubscribers at the global or per-channel limit and ErrClosed
// after Close. The subscription ends when ctx is cancelled.
func (m *Memory) Subscribe(ctx context.Context, channel Channel) (Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := channel.valid(); err != nil {
		return nil, err
	}
	key := channel.Key()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	subscribers := m.channels[key]
	if m.total >= m.opts.MaxSubscribers || len(subscribers) >= m.opts.MaxSubscribersPerChannel {
		m.mu.Unlock()
		return nil, ErrTooManySubscribers
	}
	if subscribers == nil {
		subscribers = map[*subscription]struct{}{}
		m.channels[key] = subscribers
	}
	sub := &subscription{broker: m, key: key, messages: make(chan Message, m.opts.Buffer)}
	subscribers[sub] = struct{}{}
	m.total++
	// Registered under the lock: end reads unwatch. If ctx is already
	// cancelled Close runs at once, but only after the lock is released.
	sub.unwatch = context.AfterFunc(ctx, sub.Close)
	m.mu.Unlock()
	return sub, nil
}

// removeLocked unregisters sub; m.mu must be held and sub must be live.
func (m *Memory) removeLocked(sub *subscription) {
	if subscribers := m.channels[sub.key]; subscribers != nil {
		if _, live := subscribers[sub]; live {
			delete(subscribers, sub)
			m.total--
		}
		if len(subscribers) == 0 {
			delete(m.channels, sub.key)
		}
	}
}

type subscription struct {
	broker   *Memory
	key      string
	messages chan Message
	unwatch  func() bool

	// err is written before messages is closed and read only after, so the
	// channel close orders the two.
	err  error
	done bool // guarded by broker.mu
}

func (s *subscription) Messages() <-chan Message { return s.messages }

func (s *subscription) Err() error { return s.err }

// Close ends the subscription; it is idempotent.
func (s *subscription) Close() {
	s.broker.mu.Lock()
	defer s.broker.mu.Unlock()
	s.broker.removeLocked(s)
	s.end(nil)
}

// end closes the queue once. broker.mu must be held.
func (s *subscription) end(err error) {
	if s.done {
		return
	}
	s.done = true
	s.err = err
	close(s.messages)
	if s.unwatch != nil {
		s.unwatch()
	}
}
