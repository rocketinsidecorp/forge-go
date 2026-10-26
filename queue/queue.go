// Package queue is Forge's persistent background job queue: typed JSON
// payloads, transactional enqueue, leases with token-fenced
// acknowledgements, retries with exponential backoff and jitter, dead jobs
// that operators can retry or discard, idempotency keys and graceful
// shutdown (ADR 0023).
//
// The package depends only on the standard library and Forge's core
// packages. Persistence lives in the adapters queue/postgres, queue/mysql
// and queue/sqlite, which implement Store.
//
// # Delivery contract
//
// Jobs run at least once. A process can fail after a handler's side effect
// but before the acknowledgement is stored, and a lease can expire while a
// handler is still running (a long garbage-collection pause, a partition from
// the database), after which another worker runs the job again. Handlers must
// be idempotent; use an idempotency key to avoid enqueueing the same work
// twice and the job ID (queue.JobFromContext) as the key of external side
// effects.
//
// # Payload privacy
//
// Payloads are stored in clear text and are opaque to Forge. Put identifiers
// in them, not secrets or unnecessary personal data. Payloads are never
// written to logs, hooks or error columns by the worker.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

const (
	// DefaultQueue is the queue of jobs that name none.
	DefaultQueue = "default"
	// DefaultMaxAttempts is the number of times a job runs before it is
	// declared dead when neither the handler registration nor the enqueue
	// call chooses another value. With the default backoff (5s doubling up to
	// 1h, jittered) ten attempts span roughly an hour, long enough to ride
	// out a deploy or a short downstream outage without retrying a poison
	// job for days.
	DefaultMaxAttempts = 10
	// MaxAttemptsLimit is the largest accepted attempt count.
	MaxAttemptsLimit = 1000
	// MaxPayloadBytes bounds a serialized payload to 1 MiB.
	MaxPayloadBytes = 1 << 20
	// MaxKeyLength bounds an idempotency key in bytes.
	MaxKeyLength = 200
	// MaxErrorBytes bounds the error text stored with a failed job.
	MaxErrorBytes = 1024
	// MaxPageSize bounds listings.
	MaxPageSize = 100
	// MaxPriority bounds the absolute value of a priority.
	MaxPriority = 1 << 20
)

// State is the lifecycle state of a stored job.
type State string

const (
	// StatePending jobs wait for their run time and a free worker. A job
	// retrying after a failure is pending again with a later run time.
	StatePending State = "pending"
	// StateRunning jobs are leased by a worker.
	StateRunning State = "running"
	// StateSucceeded jobs finished; Prune removes them after a retention.
	StateSucceeded State = "succeeded"
	// StateDead jobs exhausted their attempts or failed permanently. They
	// stay until an operator retries or discards them.
	StateDead State = "dead"
)

var (
	// ErrLeaseLost means another worker reclaimed the job after this
	// worker's lease expired, so this worker's acknowledgement was refused.
	ErrLeaseLost = errors.New("queue: job lease lost")
	// ErrNotFound means no job with that ID is in the state the operation
	// needs (for example, retrying a job that is not dead).
	ErrNotFound = errors.New("queue: job not found in the required state")
	// ErrKeyInUse means a dead job cannot be retried because a newer job
	// with the same kind and idempotency key is pending or running.
	ErrKeyInUse = errors.New("queue: idempotency key is held by an active job")
	// ErrUnknownKind means no handler is registered for a kind.
	ErrUnknownKind = errors.New("queue: no handler registered for kind")
	// ErrStarted reports a worker that was already started.
	ErrStarted = errors.New("queue: worker was already started")
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// ValidateName checks a job kind or queue name: 1-64 lowercase letters,
// digits, '.', '_' or '-', starting with a letter.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("name must be 1-64 lowercase letters, digits, '.', '_' or '-' starting with a letter")
	}
	return nil
}

// Job is the metadata a handler and the hooks see. It never carries the
// payload.
type Job struct {
	ID    uuid.UUID
	Kind  string
	Queue string
	// Attempt is the 1-based number of the running attempt.
	Attempt     int
	MaxAttempts int
	// Priority orders jobs of one queue; lower values run first.
	Priority   int
	RunAt      time.Time
	EnqueuedAt time.Time
	// Key is the idempotency key, or "".
	Key string
}

// NewJob is a job ready to be stored. Build it with Prepare; adapters
// validate it again before writing.
type NewJob struct {
	ID          uuid.UUID
	Kind        string
	Queue       string
	Payload     json.RawMessage
	MaxAttempts int
	Priority    int
	// RunAt is when the job becomes eligible. The zero value means as soon as
	// the storing transaction commits.
	RunAt time.Time
	// Key deduplicates active jobs of one kind; "" disables deduplication.
	Key string
}

// Validate checks every field the database would otherwise reject.
func (j NewJob) Validate() error {
	if j.ID.Version() != 7 || j.ID.Variant() != 2 {
		return errors.New("queue: job ID must be a UUIDv7")
	}
	if err := ValidateName(j.Kind); err != nil {
		return fmt.Errorf("queue: kind: %w", err)
	}
	if err := ValidateName(j.Queue); err != nil {
		return fmt.Errorf("queue: queue: %w", err)
	}
	if len(j.Payload) == 0 || len(j.Payload) > MaxPayloadBytes || !json.Valid(j.Payload) {
		return fmt.Errorf("queue: payload must be valid JSON between 1 byte and %d bytes", MaxPayloadBytes)
	}
	if j.MaxAttempts < 1 || j.MaxAttempts > MaxAttemptsLimit {
		return fmt.Errorf("queue: max attempts must be between 1 and %d", MaxAttemptsLimit)
	}
	if j.Priority < -MaxPriority || j.Priority > MaxPriority {
		return fmt.Errorf("queue: priority must be between %d and %d", -MaxPriority, MaxPriority)
	}
	if !j.RunAt.IsZero() {
		if err := validateRunAt(j.RunAt); err != nil {
			return err
		}
	}
	return validateKey(j.Key)
}

// Years every adapter stores and orders correctly: SQLite compares fixed-width
// text (a five-digit year sorts before 2026) and MySQL DATETIME ends at 9999.
const (
	minRunAtYear = 1000
	maxRunAtYear = 9999
)

func validateRunAt(t time.Time) error {
	if year := t.UTC().Year(); year < minRunAtYear || year > maxRunAtYear {
		return fmt.Errorf("queue: run time must fall in the years %d to %d", minRunAtYear, maxRunAtYear)
	}
	return nil
}

func validateKey(key string) error {
	if len(key) > MaxKeyLength {
		return fmt.Errorf("queue: idempotency key must not exceed %d bytes", MaxKeyLength)
	}
	for _, r := range key {
		if unsafeRune(r) {
			return errors.New("queue: idempotency key must be printable UTF-8 without control or bidirectional formatting characters")
		}
	}
	return nil
}

// InsertResult reports what Insert stored.
type InsertResult struct {
	// ID is the new job's ID, or the existing job's when Duplicate.
	ID uuid.UUID
	// Duplicate means an active job of the same kind already holds the
	// idempotency key; nothing was stored.
	Duplicate bool
}

// Inserter stores new jobs. Every Store is one; adapters also return an
// Inserter bound to a caller's transaction (their In method), so a job
// commits or rolls back atomically with the business data that caused it.
type Inserter interface {
	Insert(ctx context.Context, job NewJob) (InsertResult, error)
}

// Lease identifies one claim of one job. The token fences every later write:
// a worker whose lease expired and was reclaimed holds a stale token and its
// acknowledgements fail with ErrLeaseLost.
type Lease struct {
	JobID uuid.UUID
	Token uuid.UUID
}

// ClaimRequest asks the store for one eligible job.
type ClaimRequest struct {
	Queue string
	// Kinds restricts the claim to kinds this worker can handle, so a job
	// enqueued by a newer deploy waits for a worker that knows it instead of
	// failing on an old one.
	Kinds    []string
	WorkerID string
	// Lease is the initial lease duration.
	Lease time.Duration
}

// Claimed is a leased job with its payload.
type Claimed struct {
	Job     Job
	Payload json.RawMessage
	Lease   Lease
}

// Failure records how an attempt ended.
type Failure struct {
	// Message is the sanitized, bounded error text.
	Message string
	// Dead moves the job to StateDead instead of scheduling a retry.
	Dead bool
	// RetryIn delays the next attempt when !Dead.
	RetryIn time.Duration
}

// Info is the stored state of one job, without its payload.
type Info struct {
	Job        Job
	State      State
	LastError  string
	WorkerID   string
	LeaseUntil *time.Time
	FinishedAt *time.Time
}

// QueueStats is a snapshot of one queue at query time.
type QueueStats struct {
	Queue string
	// Ready jobs are pending and due; Scheduled jobs are pending for later.
	Ready     int64
	Scheduled int64
	Running   int64
	Dead      int64
	Succeeded int64
	// OldestReadyAt is the due time of the longest-waiting ready job, nil
	// when none is ready. Its age is the queue latency.
	OldestReadyAt *time.Time
}

// Stats is a snapshot of every queue.
type Stats struct {
	Queues []QueueStats
}

// DeadQuery selects dead jobs. After continues a keyset page.
type DeadQuery struct {
	Queue string
	Kind  string
	After *DeadCursor
	// Limit defaults to 25 and must not exceed MaxPageSize.
	Limit int
}

// DeadCursor continues a listing ordered by finish time then ID.
type DeadCursor struct {
	FinishedAt time.Time
	ID         uuid.UUID
}

// DeadPage is one page of dead jobs and a cursor when more exist.
type DeadPage struct {
	Items []Info
	Next  *DeadCursor
}

// Admin is the operator surface of a store: observability and the manual
// recovery of dead jobs. It needs no handlers, so command-line tools use it
// without a Registry.
type Admin interface {
	Stats(ctx context.Context) (Stats, error)
	Get(ctx context.Context, id uuid.UUID) (Info, error)
	ListDead(ctx context.Context, query DeadQuery) (DeadPage, error)
	// Retry moves a dead job back to pending with its attempts reset and
	// returns ErrNotFound if it is not dead and ErrKeyInUse if a newer
	// active job holds its idempotency key.
	Retry(ctx context.Context, id uuid.UUID) error
	// Discard permanently deletes a dead job, payload included.
	Discard(ctx context.Context, id uuid.UUID) error
	// Prune deletes up to limit succeeded jobs that finished more than
	// retention ago and reports how many it deleted.
	Prune(ctx context.Context, retention time.Duration, limit int) (int64, error)
}

// Store is what a Worker needs from a database adapter.
type Store interface {
	Inserter
	Admin
	// Claim leases the next eligible job of the request, or reports false.
	// It does not recover expired leases: Recover does, and every Worker
	// calls it on its own schedule. Code that drives a Store without a
	// Worker must call Recover itself, or jobs of crashed workers stay
	// leased forever.
	Claim(ctx context.Context, request ClaimRequest) (Claimed, bool, error)
	// Renew extends the lease to now+extend, or returns ErrLeaseLost.
	Renew(ctx context.Context, lease Lease, extend time.Duration) error
	// Complete marks the job succeeded, or returns ErrLeaseLost.
	Complete(ctx context.Context, lease Lease) error
	// Fail schedules a retry or kills the job, or returns ErrLeaseLost.
	Fail(ctx context.Context, lease Lease, failure Failure) error
	// Recover makes jobs whose lease expired pending again, or dead when they
	// already used their last attempt, and reports how many it changed. One
	// call handles a bounded batch (RecoverBatch jobs); workers call it
	// every Lease/4, off the claim path, and repeat it while batches are
	// full.
	Recover(ctx context.Context) (int64, error)
	// Release gives the job back without penalty (the attempt is not
	// counted), or returns ErrLeaseLost. Graceful shutdown uses it.
	Release(ctx context.Context, lease Lease) error
}

// RecoverBatch is the most jobs one Store.Recover call changes.
const RecoverBatch = 100

// MaxClaimBatch is the most jobs one ClaimBatch call may lease.
const MaxClaimBatch = 100

// BatchClaimer is implemented by stores that lease several jobs in one round
// trip. A Worker uses it when present and falls back to Claim otherwise.
type BatchClaimer interface {
	// ClaimBatch leases up to limit (1 to MaxClaimBatch) eligible jobs of the
	// request, the ones Claim would return one by one, and returns them,
	// possibly none, in no particular order (UPDATE ... RETURNING does not
	// keep one). Each job is leased exactly as Claim leases one:
	// same lease duration, attempt count and token fencing. The jobs may
	// share one lease token; acknowledgements stay fenced because a lease
	// names the job and the token.
	ClaimBatch(ctx context.Context, request ClaimRequest, limit int) ([]Claimed, error)
}

// ErrListenUnsupported is what Listener.Listen returns when the store was not
// configured to publish enqueue notifications. A worker then stops listening
// and only polls.
var ErrListenUnsupported = errors.New("queue: the store does not publish enqueue notifications")

// Listener is implemented by stores that announce enqueued jobs, such as
// queue/postgres with WithNotify (LISTEN/NOTIFY). A Worker whose store is a
// Listener holds one listening connection and claims as soon as a job of one
// of its queues is announced, instead of waiting for its next poll. Polling
// continues as the fallback: announcements are hints, may be lost while the
// connection is down, and are never needed for correctness.
type Listener interface {
	// Listen subscribes to announcements and blocks until ctx ends or the
	// subscription fails, which it reports as an error (ErrListenUnsupported
	// when the store does not announce). It calls listening once the
	// subscription is active and wake with the queue name of each
	// announcement, both on the calling goroutine; wake must not block. It
	// must return promptly when ctx ends and release its connection.
	Listen(ctx context.Context, listening func(), wake func(queue string)) error
}

type jobContextKey struct{}

// JobFromContext returns the metadata of the job a handler is running.
func JobFromContext(ctx context.Context) (Job, bool) {
	job, ok := ctx.Value(jobContextKey{}).(Job)
	return job, ok
}
