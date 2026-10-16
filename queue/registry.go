package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// maxHandlerTimeout bounds a handler timeout option.
const maxHandlerTimeout = 24 * time.Hour

// Registry maps job kinds to typed handlers. It is safe for concurrent use;
// register every handler before starting a Worker so the worker claims only
// kinds it can run.
type Registry struct {
	mu     sync.RWMutex
	byKind map[string]*registration
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byKind: make(map[string]*registration)}
}

type registration struct {
	kind        string
	payloadType reflect.Type
	run         func(ctx context.Context, payload json.RawMessage) error
	settings    handlerSettings
}

type handlerSettings struct {
	queue       string
	maxAttempts int
	priority    int
	timeout     time.Duration
	backoff     Backoff
}

// HandlerOption sets defaults for every job of a kind.
type HandlerOption func(*handlerSettings) error

// HandlerQueue sets the queue jobs of the kind use unless an enqueue call
// names another. The default is DefaultQueue.
func HandlerQueue(name string) HandlerOption {
	return func(s *handlerSettings) error {
		if err := ValidateName(name); err != nil {
			return fmt.Errorf("queue: handler queue: %w", err)
		}
		s.queue = name
		return nil
	}
}

// HandlerMaxAttempts sets how many times a job of the kind runs before it is
// declared dead. The default is DefaultMaxAttempts.
func HandlerMaxAttempts(n int) HandlerOption {
	return func(s *handlerSettings) error {
		if n < 1 || n > MaxAttemptsLimit {
			return fmt.Errorf("queue: handler max attempts must be between 1 and %d", MaxAttemptsLimit)
		}
		s.maxAttempts = n
		return nil
	}
}

// HandlerPriority sets the default priority of the kind; lower runs first.
func HandlerPriority(priority int) HandlerOption {
	return func(s *handlerSettings) error {
		if priority < -MaxPriority || priority > MaxPriority {
			return fmt.Errorf("queue: handler priority must be between %d and %d", -MaxPriority, MaxPriority)
		}
		s.priority = priority
		return nil
	}
}

// HandlerTimeout bounds one attempt: the handler's context ends after d and
// the attempt fails. Without it an attempt runs until it returns, with its
// lease renewed meanwhile, so set a timeout for work that can hang.
func HandlerTimeout(d time.Duration) HandlerOption {
	return func(s *handlerSettings) error {
		if d <= 0 || d > maxHandlerTimeout {
			return fmt.Errorf("queue: handler timeout must be positive and at most %s", maxHandlerTimeout)
		}
		s.timeout = d
		return nil
	}
}

// HandlerBackoff overrides the worker's retry backoff for the kind.
func HandlerBackoff(b Backoff) HandlerOption {
	return func(s *handlerSettings) error {
		if err := b.validate(); err != nil {
			return err
		}
		s.backoff = b
		return nil
	}
}

// Register adds the handler for kind. The payload type T fixes the job's
// schema: Prepare and Enqueue accept only T for the kind, the payload is
// stored as JSON, and the worker decodes it strictly (unknown fields and
// trailing data are errors) before calling handler. A payload that no longer
// decodes fails permanently, because retrying cannot help.
//
// Handlers run at least once and must be idempotent; see the package
// documentation.
func Register[T any](r *Registry, kind string, handler func(ctx context.Context, payload T) error, opts ...HandlerOption) error {
	if r == nil {
		return errors.New("queue: registry is required")
	}
	if err := ValidateName(kind); err != nil {
		return fmt.Errorf("queue: kind %q: %w", kind, err)
	}
	if handler == nil {
		return errors.New("queue: handler is required")
	}
	settings := handlerSettings{queue: DefaultQueue, maxAttempts: DefaultMaxAttempts}
	for _, opt := range opts {
		if opt == nil {
			return errors.New("queue: nil handler option")
		}
		if err := opt(&settings); err != nil {
			return err
		}
	}
	reg := &registration{
		kind:        kind,
		payloadType: reflect.TypeFor[T](),
		settings:    settings,
		run: func(ctx context.Context, raw json.RawMessage) error {
			payload, err := decodeStrict[T](raw)
			if err != nil {
				return Permanent(fmt.Errorf("decode %s payload: %w", kind, err))
			}
			return handler(ctx, payload)
		},
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byKind[kind]; exists {
		return fmt.Errorf("queue: kind %q is already registered", kind)
	}
	r.byKind[kind] = reg
	return nil
}

func (r *Registry) lookup(kind string) (*registration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.byKind[kind]
	return reg, ok
}

// Kinds returns the registered kinds, sorted.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	kinds := make([]string, 0, len(r.byKind))
	for kind := range r.byKind {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

// decodeStrict decodes exactly one JSON value into T, rejecting unknown
// fields and trailing data. Its errors name fields and types, never values.
func decodeStrict[T any](data []byte) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return value, errors.New("unexpected data after the JSON payload")
	}
	return value, nil
}

type enqueueSettings struct {
	queue       string
	maxAttempts int
	priority    *int
	runAt       time.Time
	key         string
	err         error
}

// EnqueueOption customizes one job.
type EnqueueOption func(*enqueueSettings)

// WithQueue puts the job on a named queue instead of its kind's default.
func WithQueue(name string) EnqueueOption {
	return func(s *enqueueSettings) {
		if err := ValidateName(name); err != nil {
			s.err = fmt.Errorf("queue: queue: %w", err)
			return
		}
		s.queue = name
	}
}

// WithMaxAttempts overrides the kind's attempt budget for this job.
func WithMaxAttempts(n int) EnqueueOption {
	return func(s *enqueueSettings) {
		if n < 1 || n > MaxAttemptsLimit {
			s.err = fmt.Errorf("queue: max attempts must be between 1 and %d", MaxAttemptsLimit)
			return
		}
		s.maxAttempts = n
	}
}

// WithPriority overrides the kind's priority for this job; lower runs first.
func WithPriority(priority int) EnqueueOption {
	return func(s *enqueueSettings) {
		if priority < -MaxPriority || priority > MaxPriority {
			s.err = fmt.Errorf("queue: priority must be between %d and %d", -MaxPriority, MaxPriority)
			return
		}
		s.priority = &priority
	}
}

// RunAt schedules the job for an absolute time.
func RunAt(t time.Time) EnqueueOption {
	return func(s *enqueueSettings) {
		if t.IsZero() {
			s.err = errors.New("queue: run time is required")
			return
		}
		if err := validateRunAt(t); err != nil {
			s.err = err
			return
		}
		s.runAt = t.UTC()
	}
}

// RunAfter schedules the job d from now (by the caller's clock).
func RunAfter(d time.Duration) EnqueueOption {
	return func(s *enqueueSettings) {
		if d < 0 {
			s.err = errors.New("queue: delay must not be negative")
			return
		}
		s.runAt = time.Now().Add(d).UTC()
	}
}

// WithKey sets an idempotency key. While a job of the same kind with the same
// key is pending or running, enqueueing again stores nothing and reports
// InsertResult.Duplicate. Once that job succeeds or dies the key is free.
func WithKey(key string) EnqueueOption {
	return func(s *enqueueSettings) {
		if key == "" {
			s.err = errors.New("queue: idempotency key must not be empty")
			return
		}
		if err := validateKey(key); err != nil {
			s.err = err
			return
		}
		s.key = key
	}
}

// Prepare validates payload against the type registered for kind, encodes it
// and applies the kind's defaults and the options. The result is stored with
// any Inserter, normally through Enqueue.
func Prepare[T any](r *Registry, kind string, payload T, opts ...EnqueueOption) (NewJob, error) {
	if r == nil {
		return NewJob{}, errors.New("queue: registry is required")
	}
	reg, ok := r.lookup(kind)
	if !ok {
		return NewJob{}, fmt.Errorf("%w %q", ErrUnknownKind, kind)
	}
	if want := reflect.TypeFor[T](); reg.payloadType != want {
		return NewJob{}, fmt.Errorf("queue: kind %q takes payload type %s, not %s", kind, reg.payloadType, want)
	}
	settings := enqueueSettings{}
	for _, opt := range opts {
		if opt == nil {
			return NewJob{}, errors.New("queue: nil enqueue option")
		}
		opt(&settings)
		if settings.err != nil {
			return NewJob{}, settings.err
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return NewJob{}, fmt.Errorf("queue: encode %s payload: %w", kind, err)
	}
	if bytes.Equal(data, []byte("null")) {
		return NewJob{}, fmt.Errorf("queue: %s payload encodes to null", kind)
	}
	// What is stored must be what the worker can decode; check it now, where
	// the programmer can fix it, instead of dead-lettering it later.
	if _, err := decodeStrict[T](data); err != nil {
		return NewJob{}, fmt.Errorf("queue: %s payload does not round-trip through its strict decoder: %w", kind, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return NewJob{}, fmt.Errorf("queue: generate job ID: %w", err)
	}
	job := NewJob{
		ID:          id,
		Kind:        kind,
		Queue:       reg.settings.queue,
		Payload:     data,
		MaxAttempts: reg.settings.maxAttempts,
		Priority:    reg.settings.priority,
		RunAt:       settings.runAt,
		Key:         settings.key,
	}
	if settings.queue != "" {
		job.Queue = settings.queue
	}
	if settings.maxAttempts != 0 {
		job.MaxAttempts = settings.maxAttempts
	}
	if settings.priority != nil {
		job.Priority = *settings.priority
	}
	if err := job.Validate(); err != nil {
		return NewJob{}, err
	}
	return job, nil
}

// Enqueue prepares a job and stores it with in, which is a Store for an
// independent write or the Inserter an adapter binds to the caller's
// transaction for an atomic one:
//
//	err := db.InTx(ctx, func(tx pgx.Tx) error {
//	    if err := createOrder(ctx, tx, order); err != nil { return err }
//	    _, err := queue.Enqueue(ctx, store.In(tx), registry, "order.confirm", ConfirmOrder{OrderID: order.ID})
//	    return err
//	})
//
// The job exists if and only if the transaction commits.
func Enqueue[T any](ctx context.Context, in Inserter, r *Registry, kind string, payload T, opts ...EnqueueOption) (InsertResult, error) {
	if in == nil {
		return InsertResult{}, errors.New("queue: inserter is required")
	}
	job, err := Prepare(r, kind, payload, opts...)
	if err != nil {
		return InsertResult{}, err
	}
	return in.Insert(ctx, job)
}
