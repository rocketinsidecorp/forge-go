// Package sqlite is the SQLite adapter of Forge's job queue (ADR 0023).
//
// SQLite has one writer at a time and no SKIP LOCKED; the adapter does not
// need either. A claim is a single UPDATE ... RETURNING statement, which the
// database executes atomically under its write lock, so concurrent workers of
// one process or of several processes on the same host never claim a job
// twice. Every acknowledgement is fenced by the claim's lease token.
//
// SQLite serves one application instance on one host (see package sqlite):
// the database file must not be on a network file system. Lease timestamps
// use the application's clock, which is the only clock when all workers share
// a host. The adapter suits development, tests, single-node deployments and
// small workloads; use PostgreSQL or MySQL to run workers on several hosts.
//
// Enqueue inside the caller's transaction with Store.In:
//
//	err := db.InTx(ctx, func(tx *sql.Tx) error {
//	    // ... business writes using tx ...
//	    _, err := queue.Enqueue(ctx, store.In(tx), registry, "invoice.send", SendInvoice{InvoiceID: id})
//	    return err
//	})
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	sqlite3 "modernc.org/sqlite/lib"

	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	forgesqlite "github.com/rocketinsidecorp/forge-go/sqlite"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// timeFormat is a fixed-width UTC timestamp, so TEXT columns sort and compare
// chronologically.
const timeFormat = "2006-01-02 15:04:05.000000"

// recoveryInterval spaces lease recovery sweeps per store, so a hot claim
// path pays for one extra statement per interval, not per claim. A crashed
// worker's job waits at most this long beyond its lease.
const recoveryInterval = 250 * time.Millisecond

// Store is the SQLite implementation of queue.Store.
type Store struct {
	db       *sqldb.DB
	now      func() time.Time
	lastReap atomic.Int64 // unix nanoseconds of the last lease recovery
}

var _ queue.Store = (*Store)(nil)

// NewStore returns a store on db, which sqlite.Open returns. The tables come
// from Migrations.
func NewStore(db *sqldb.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("queue SQLite database is required")
	}
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}, nil
}

func format(t time.Time) string { return t.UTC().Format(timeFormat) }

func parse(value string) (time.Time, error) {
	t, err := time.ParseInLocation(timeFormat, value, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time %q: %w", value, err)
	}
	return t, nil
}

func parseNullable(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	t, err := parse(value.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// In returns an Inserter that writes through tx, so the job commits or rolls
// back with the caller's transaction and is invisible to workers until then.
// tx is normally the *sql.Tx of sqldb.DB.InTx; nothing is committed by the
// store.
func (s *Store) In(tx sqldb.DBTX) queue.Inserter { return inserter{exec: tx, now: s.now} }

type inserter struct {
	exec sqldb.DBTX
	now  func() time.Time
}

// Insert stores job in its own implicit transaction (a single statement).
// Use In for atomicity with business data.
func (s *Store) Insert(ctx context.Context, job queue.NewJob) (queue.InsertResult, error) {
	return inserter{exec: s.db, now: s.now}.Insert(ctx, job)
}

func (in inserter) Insert(ctx context.Context, job queue.NewJob) (queue.InsertResult, error) {
	if in.exec == nil {
		return queue.InsertResult{}, errors.New("queue transaction is required")
	}
	if err := job.Validate(); err != nil {
		return queue.InsertResult{}, err
	}
	now := in.now()
	runAt := job.RunAt
	if runAt.IsZero() {
		runAt = now
	}
	var key any
	if job.Key != "" {
		key = job.Key
	}
	const insertSQL = `
		INSERT INTO forge_jobs (id, queue, kind, payload, priority, max_attempts, run_at, enqueued_at, idempotency_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, idempotency_key) WHERE idempotency_key IS NOT NULL AND state IN ('pending', 'running') DO NOTHING
		RETURNING id`
	const existingSQL = `
		SELECT id FROM forge_jobs
		WHERE kind = ? AND idempotency_key = ? AND state IN ('pending', 'running')`
	for range 3 {
		var id uuid.UUID
		err := in.exec.QueryRowContext(ctx, insertSQL, job.ID, job.Queue, job.Kind, string(job.Payload), job.Priority, job.MaxAttempts,
			format(runAt), format(now), key).Scan(&id)
		if err == nil {
			return queue.InsertResult{ID: id}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return queue.InsertResult{}, forgesqlite.Translate(fmt.Errorf("insert job: %w", err))
		}
		err = in.exec.QueryRowContext(ctx, existingSQL, job.Kind, job.Key).Scan(&id)
		if err == nil {
			return queue.InsertResult{ID: id, Duplicate: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return queue.InsertResult{}, forgesqlite.Translate(fmt.Errorf("find job holding idempotency key: %w", err))
		}
		// The holder finished between the two statements; insert again.
	}
	return queue.InsertResult{}, errors.New("queue: idempotency key conflict could not be resolved")
}

func inClause(n int) string {
	return "(" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}

// Claim leases the next eligible job of the request.
func (s *Store) Claim(ctx context.Context, request queue.ClaimRequest) (queue.Claimed, bool, error) {
	if request.Queue == "" || len(request.Kinds) == 0 || request.Lease <= 0 {
		return queue.Claimed{}, false, errors.New("queue claim needs a queue, kinds and a lease")
	}
	if err := s.recoverExpired(ctx); err != nil {
		return queue.Claimed{}, false, err
	}
	token, err := uuid.NewV7()
	if err != nil {
		return queue.Claimed{}, false, fmt.Errorf("generate lease token: %w", err)
	}
	now := s.now()
	args := []any{token, format(now.Add(request.Lease)), request.WorkerID, request.Queue}
	for _, kind := range request.Kinds {
		args = append(args, kind)
	}
	args = append(args, format(now))
	var (
		job             queue.Job
		payload         string
		runAt, enqueued string
	)
	err = s.db.QueryRowContext(ctx, `
		UPDATE forge_jobs
		SET state = 'running', attempts = attempts + 1, lease_token = ?, lease_until = ?, locked_by = ?
		WHERE id = (
			SELECT id FROM forge_jobs
			WHERE state = 'pending' AND queue = ? AND kind IN `+inClause(len(request.Kinds))+` AND run_at <= ?
			ORDER BY priority, run_at, id
			LIMIT 1
		)
		RETURNING id, kind, queue, payload, attempts, max_attempts, priority, run_at, enqueued_at, COALESCE(idempotency_key, '')
	`, args...).Scan(&job.ID, &job.Kind, &job.Queue, &payload, &job.Attempt, &job.MaxAttempts, &job.Priority, &runAt, &enqueued, &job.Key)
	if errors.Is(err, sql.ErrNoRows) {
		return queue.Claimed{}, false, nil
	}
	if err != nil {
		return queue.Claimed{}, false, fmt.Errorf("claim job: %w", err)
	}
	if job.RunAt, err = parse(runAt); err != nil {
		return queue.Claimed{}, false, err
	}
	if job.EnqueuedAt, err = parse(enqueued); err != nil {
		return queue.Claimed{}, false, err
	}
	return queue.Claimed{Job: job, Payload: []byte(payload), Lease: queue.Lease{JobID: job.ID, Token: token}}, true, nil
}

// recoverExpired is Recover, at most once per recoveryInterval, for Claim.
func (s *Store) recoverExpired(ctx context.Context) error {
	now := time.Now().UnixNano()
	last := s.lastReap.Load()
	if now-last < int64(recoveryInterval) || !s.lastReap.CompareAndSwap(last, now) {
		return nil
	}
	_, err := s.Recover(ctx)
	return err
}

// Recover makes jobs whose lease expired pending again, or dead when they
// used their last attempt. It handles up to 100 jobs per call.
func (s *Store) Recover(ctx context.Context) (int64, error) {
	s.lastReap.Store(time.Now().UnixNano())
	stamp := format(s.now())
	result, err := s.db.ExecContext(ctx, `
		UPDATE forge_jobs
		SET state = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END,
		    finished_at = CASE WHEN attempts >= max_attempts THEN ? END,
		    lease_token = NULL, lease_until = NULL, locked_by = NULL,
		    last_error = 'lease expired: the worker stopped renewing it'
		WHERE id IN (
			SELECT id FROM forge_jobs
			WHERE state = 'running' AND lease_until <= ?
			ORDER BY lease_until
			LIMIT 100
		)
	`, stamp, stamp)
	if err != nil {
		return 0, fmt.Errorf("recover expired job leases: %w", err)
	}
	return result.RowsAffected()
}

// fenced runs an UPDATE guarded by id and lease token; no row means the lease
// is gone.
func (s *Store) fenced(ctx context.Context, what, query string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n != 1 {
		return queue.ErrLeaseLost
	}
	return nil
}

// Renew extends the lease of a running job.
func (s *Store) Renew(ctx context.Context, lease queue.Lease, extend time.Duration) error {
	return s.fenced(ctx, "renew job lease", `
		UPDATE forge_jobs SET lease_until = ?
		WHERE id = ? AND lease_token = ? AND state = 'running'
	`, format(s.now().Add(extend)), lease.JobID, lease.Token)
}

// Complete marks the job succeeded.
func (s *Store) Complete(ctx context.Context, lease queue.Lease) error {
	return s.fenced(ctx, "complete job", `
		UPDATE forge_jobs
		SET state = 'succeeded', finished_at = ?, lease_token = NULL, lease_until = NULL, locked_by = NULL
		WHERE id = ? AND lease_token = ? AND state = 'running'
	`, format(s.now()), lease.JobID, lease.Token)
}

// Fail schedules a retry, or kills the job when failure.Dead.
func (s *Store) Fail(ctx context.Context, lease queue.Lease, failure queue.Failure) error {
	now := s.now()
	if failure.Dead {
		return s.fenced(ctx, "fail job", `
			UPDATE forge_jobs
			SET state = 'dead', finished_at = ?, last_error = ?, lease_token = NULL, lease_until = NULL, locked_by = NULL
			WHERE id = ? AND lease_token = ? AND state = 'running'
		`, format(now), failure.Message, lease.JobID, lease.Token)
	}
	return s.fenced(ctx, "schedule job retry", `
		UPDATE forge_jobs
		SET state = 'pending', run_at = ?, last_error = ?, lease_token = NULL, lease_until = NULL, locked_by = NULL
		WHERE id = ? AND lease_token = ? AND state = 'running'
	`, format(now.Add(failure.RetryIn)), failure.Message, lease.JobID, lease.Token)
}

// Release returns the job to the queue without counting the attempt.
func (s *Store) Release(ctx context.Context, lease queue.Lease) error {
	return s.fenced(ctx, "release job", `
		UPDATE forge_jobs
		SET state = 'pending', attempts = MAX(attempts - 1, 0), lease_token = NULL, lease_until = NULL, locked_by = NULL
		WHERE id = ? AND lease_token = ? AND state = 'running'
	`, lease.JobID, lease.Token)
}

// Stats returns per-queue counts.
func (s *Store) Stats(ctx context.Context) (queue.Stats, error) {
	now := format(s.now())
	rows, err := s.db.QueryContext(ctx, `
		SELECT queue,
		       COUNT(CASE WHEN state = 'pending' AND run_at <= ?1 THEN 1 END),
		       COUNT(CASE WHEN state = 'pending' AND run_at > ?1 THEN 1 END),
		       COUNT(CASE WHEN state = 'running' THEN 1 END),
		       COUNT(CASE WHEN state = 'dead' THEN 1 END),
		       COUNT(CASE WHEN state = 'succeeded' THEN 1 END),
		       MIN(CASE WHEN state = 'pending' AND run_at <= ?1 THEN run_at END)
		FROM forge_jobs
		GROUP BY queue
		ORDER BY queue`, now)
	if err != nil {
		return queue.Stats{}, fmt.Errorf("read job queue stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var stats queue.Stats
	for rows.Next() {
		var q queue.QueueStats
		var oldest sql.NullString
		if err := rows.Scan(&q.Queue, &q.Ready, &q.Scheduled, &q.Running, &q.Dead, &q.Succeeded, &oldest); err != nil {
			return queue.Stats{}, fmt.Errorf("scan job queue stats: %w", err)
		}
		if q.OldestReadyAt, err = parseNullable(oldest); err != nil {
			return queue.Stats{}, err
		}
		stats.Queues = append(stats.Queues, q)
	}
	if err := rows.Err(); err != nil {
		return queue.Stats{}, fmt.Errorf("iterate job queue stats: %w", err)
	}
	return stats, nil
}

const infoColumns = `id, kind, queue, state, attempts, max_attempts, priority, run_at, enqueued_at,
	COALESCE(idempotency_key, ''), COALESCE(last_error, ''), COALESCE(locked_by, ''), lease_until, finished_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanInfo(row rowScanner) (queue.Info, error) {
	var info queue.Info
	var state, runAt, enqueuedAt string
	var leaseUntil, finishedAt sql.NullString
	err := row.Scan(&info.Job.ID, &info.Job.Kind, &info.Job.Queue, &state, &info.Job.Attempt, &info.Job.MaxAttempts,
		&info.Job.Priority, &runAt, &enqueuedAt, &info.Job.Key, &info.LastError, &info.WorkerID, &leaseUntil, &finishedAt)
	if err != nil {
		return queue.Info{}, err
	}
	info.State = queue.State(state)
	if info.Job.RunAt, err = parse(runAt); err != nil {
		return queue.Info{}, err
	}
	if info.Job.EnqueuedAt, err = parse(enqueuedAt); err != nil {
		return queue.Info{}, err
	}
	if info.LeaseUntil, err = parseNullable(leaseUntil); err != nil {
		return queue.Info{}, err
	}
	if info.FinishedAt, err = parseNullable(finishedAt); err != nil {
		return queue.Info{}, err
	}
	return info, nil
}

// Get returns one job's metadata, never its payload.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (queue.Info, error) {
	info, err := scanInfo(s.db.QueryRowContext(ctx, "SELECT "+infoColumns+" FROM forge_jobs WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return queue.Info{}, queue.ErrNotFound
	}
	if err != nil {
		return queue.Info{}, fmt.Errorf("read job: %w", err)
	}
	return info, nil
}

// ListDead returns a page of dead jobs ordered by finish time then ID.
func (s *Store) ListDead(ctx context.Context, query queue.DeadQuery) (queue.DeadPage, error) {
	limit := query.Limit
	if limit == 0 {
		limit = 25
	}
	if limit < 1 || limit > queue.MaxPageSize {
		return queue.DeadPage{}, fmt.Errorf("dead job page size must be between 1 and %d", queue.MaxPageSize)
	}
	statement := "SELECT " + infoColumns + " FROM forge_jobs WHERE state = 'dead'"
	var args []any
	if query.Queue != "" {
		statement += " AND queue = ?"
		args = append(args, query.Queue)
	}
	if query.Kind != "" {
		statement += " AND kind = ?"
		args = append(args, query.Kind)
	}
	if query.After != nil {
		after := format(query.After.FinishedAt)
		statement += " AND (finished_at > ? OR (finished_at = ? AND id > ?))"
		args = append(args, after, after, query.After.ID)
	}
	statement += " ORDER BY finished_at, id LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return queue.DeadPage{}, fmt.Errorf("list dead jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]queue.Info, 0, limit+1)
	for rows.Next() {
		info, err := scanInfo(rows)
		if err != nil {
			return queue.DeadPage{}, fmt.Errorf("scan dead job: %w", err)
		}
		items = append(items, info)
	}
	if err := rows.Err(); err != nil {
		return queue.DeadPage{}, fmt.Errorf("iterate dead jobs: %w", err)
	}
	page := queue.DeadPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		page.Next = &queue.DeadCursor{FinishedAt: *last.FinishedAt, ID: last.Job.ID}
	}
	return page, nil
}

// Retry moves a dead job back to pending with its attempts reset.
func (s *Store) Retry(ctx context.Context, id uuid.UUID) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE forge_jobs
		SET state = 'pending', attempts = 0, run_at = ?, finished_at = NULL
		WHERE id = ? AND state = 'dead'
	`, format(s.now()), id)
	if err != nil {
		if forgesqlite.ErrorCode(err) == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
			return queue.ErrKeyInUse
		}
		return fmt.Errorf("retry dead job: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return fmt.Errorf("retry dead job: %w", err)
		}
		return queue.ErrNotFound
	}
	return nil
}

// Discard permanently deletes a dead job.
func (s *Store) Discard(ctx context.Context, id uuid.UUID) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM forge_jobs WHERE id = ? AND state = 'dead'", id)
	if err != nil {
		return fmt.Errorf("discard dead job: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return fmt.Errorf("discard dead job: %w", err)
		}
		return queue.ErrNotFound
	}
	return nil
}

// Prune deletes up to limit succeeded jobs finished before now-retention.
func (s *Store) Prune(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	if retention < 0 || limit < 1 {
		return 0, errors.New("prune needs a non-negative retention and a positive limit")
	}
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM forge_jobs
		WHERE id IN (
			SELECT id FROM forge_jobs
			WHERE state = 'succeeded' AND finished_at < ?
			ORDER BY finished_at
			LIMIT ?
		)
	`, format(s.now().Add(-retention)), limit)
	if err != nil {
		return 0, fmt.Errorf("prune finished jobs: %w", err)
	}
	return result.RowsAffected()
}
