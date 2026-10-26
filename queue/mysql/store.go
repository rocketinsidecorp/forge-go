// Package mysql is the MySQL and MariaDB adapter of Forge's job queue
// (ADR 0023). It is tested on MySQL 8.4 and MariaDB 11.8, both of which
// support SELECT ... FOR UPDATE SKIP LOCKED.
//
// A claim is one short READ COMMITTED transaction (select the next due jobs,
// several at a time with ClaimBatch, with SKIP LOCKED, mark them running)
// that ends before the handlers start, so
// no transaction or row lock is held while a handler runs. READ COMMITTED
// avoids the gap locks REPEATABLE READ would take on the claim index, which
// would otherwise block concurrent enqueues. All lease timestamps come from
// the database clock. Every acknowledgement is fenced by the claim's lease
// token.
//
// Enqueue inside the caller's transaction with Store.In:
//
//	err := db.InTx(ctx, func(tx *sql.Tx) error {
//	    // ... business writes using tx ...
//	    _, err := queue.Enqueue(ctx, store.In(tx), registry, "invoice.send", SendInvoice{InvoiceID: id})
//	    return err
//	})
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	forgemysql "github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// duplicateEntry is MySQL and MariaDB error 1062 (ER_DUP_ENTRY).
const duplicateEntry = 1062

// claimAttempts reruns a claim transaction that lost a deadlock.
const claimAttempts = 3

// Store is the MySQL/MariaDB implementation of queue.Store.
type Store struct {
	db *sqldb.DB
}

var (
	_ queue.Store        = (*Store)(nil)
	_ queue.BatchClaimer = (*Store)(nil)
)

// NewStore returns a store on db, which mysql.Open returns. The tables come
// from Migrations.
func NewStore(db *sqldb.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("queue MySQL database is required")
	}
	return &Store{db: db}, nil
}

// In returns an Inserter that writes through tx, so the job commits or rolls
// back with the caller's transaction and is invisible to workers until then.
// tx is normally the *sql.Tx of sqldb.DB.InTx; nothing is committed by the
// store.
func (s *Store) In(tx sqldb.DBTX) queue.Inserter { return inserter{exec: tx, reader: s.db} }

// inserter writes through exec (the caller's transaction) and reads the
// committed state of the queue through reader (the pool), outside that
// transaction.
type inserter struct{ exec, reader sqldb.DBTX }

// Insert stores job in its own implicit transaction (a single statement).
// Use In for atomicity with business data.
func (s *Store) Insert(ctx context.Context, job queue.NewJob) (queue.InsertResult, error) {
	return inserter{exec: s.db, reader: s.db}.Insert(ctx, job)
}

func (in inserter) Insert(ctx context.Context, job queue.NewJob) (queue.InsertResult, error) {
	if in.exec == nil {
		return queue.InsertResult{}, errors.New("queue transaction is required")
	}
	if err := job.Validate(); err != nil {
		return queue.InsertResult{}, err
	}
	runAt := "UTC_TIMESTAMP(6)"
	args := []any{job.ID, job.Queue, job.Kind, string(job.Payload), job.Priority, job.MaxAttempts}
	if !job.RunAt.IsZero() {
		runAt = "?"
		args = append(args, job.RunAt.UTC())
	}
	var key any
	if job.Key != "" {
		key = job.Key
	}
	args = append(args, key)
	insertSQL := `INSERT INTO forge_jobs (id, queue, kind, payload, priority, max_attempts, run_at, enqueued_at, idempotency_key)
		VALUES (?, ?, ?, ?, ?, ?, ` + runAt + `, UTC_TIMESTAMP(6), ?)`
	if job.Key == "" {
		if _, err := in.exec.ExecContext(ctx, insertSQL, args...); err != nil {
			return queue.InsertResult{}, forgemysql.Translate(fmt.Errorf("insert job: %w", err))
		}
		return queue.InsertResult{ID: job.ID}, nil
	}

	// A keyed enqueue first looks for the holder WITHOUT locking it. Locking
	// reads and ON DUPLICATE KEY UPDATE would take row locks on the active
	// job that last until the caller's transaction ends, blocking the
	// worker's heartbeat and acknowledgement of a job that is running.
	for range 3 {
		var id uuid.UUID
		err := in.exec.QueryRowContext(ctx, "SELECT id FROM forge_jobs WHERE kind = ? AND active_key = ?", job.Kind, job.Key).Scan(&id)
		if err == nil {
			return queue.InsertResult{ID: id, Duplicate: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return queue.InsertResult{}, forgemysql.Translate(fmt.Errorf("find job holding idempotency key: %w", err))
		}
		_, err = in.exec.ExecContext(ctx, insertSQL, args...)
		if err == nil {
			return queue.InsertResult{ID: job.ID}, nil
		}
		if forgemysql.ErrorNumber(err) != duplicateEntry {
			return queue.InsertResult{}, forgemysql.Translate(fmt.Errorf("insert job: %w", err))
		}
		// Lost a race: a concurrent enqueue committed first, or the snapshot
		// of a REPEATABLE READ transaction hid a committed holder. Read the
		// committed holder through the pool, outside the caller's
		// transaction, so no locking read is needed. The failed INSERT still
		// leaves a shared lock on the duplicate unique-index entry until the
		// caller's transaction ends; it can delay the holder's Complete or Fail
		// (they clear the entry) but never a heartbeat. It exists only when two
		// enqueues of one key race, never for an ordinary duplicate.
		err = in.reader.QueryRowContext(ctx, "SELECT id FROM forge_jobs WHERE kind = ? AND active_key = ?", job.Kind, job.Key).Scan(&id)
		if err == nil {
			return queue.InsertResult{ID: id, Duplicate: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return queue.InsertResult{}, forgemysql.Translate(fmt.Errorf("find job holding idempotency key: %w", err))
		}
		// The holder finished meanwhile; try again.
	}
	return queue.InsertResult{}, errors.New("queue: idempotency key conflict could not be resolved")
}

func inClause(n int) string {
	return "(" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}

// Claim leases the next eligible job of the request.
func (s *Store) Claim(ctx context.Context, request queue.ClaimRequest) (queue.Claimed, bool, error) {
	claimed, err := s.ClaimBatch(ctx, request, 1)
	if err != nil || len(claimed) == 0 {
		return queue.Claimed{}, false, err
	}
	return claimed[0], true, nil
}

// ClaimBatch leases up to limit eligible jobs of the request in one READ
// COMMITTED transaction. SKIP LOCKED makes concurrent claims take disjoint
// jobs; the jobs share one lease token.
func (s *Store) ClaimBatch(ctx context.Context, request queue.ClaimRequest, limit int) ([]queue.Claimed, error) {
	if request.Queue == "" || len(request.Kinds) == 0 || request.Lease <= 0 {
		return nil, errors.New("queue claim needs a queue, kinds and a lease")
	}
	if limit < 1 || limit > queue.MaxClaimBatch {
		return nil, fmt.Errorf("queue claim batch must be between 1 and %d", queue.MaxClaimBatch)
	}
	token, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate lease token: %w", err)
	}
	args := []any{request.Queue}
	for _, kind := range request.Kinds {
		args = append(args, kind)
	}
	args = append(args, limit)
	selectSQL := `
		SELECT id, kind, queue, payload, attempts, max_attempts, priority, run_at, enqueued_at, COALESCE(idempotency_key, '')
		FROM forge_jobs
		WHERE state = 'pending' AND queue = ? AND kind IN ` + inClause(len(request.Kinds)) + `
		  AND run_at <= UTC_TIMESTAMP(6)
		ORDER BY priority, run_at, id
		LIMIT ?
		FOR UPDATE SKIP LOCKED` // #nosec G202 -- inClause emits only "?" placeholders
	var claimed []queue.Claimed
	err = s.db.InTxWith(ctx, sqldb.TxOptions{Isolation: sql.LevelReadCommitted, Attempts: claimAttempts}, func(tx *sql.Tx) error {
		claimed = nil
		rows, err := tx.QueryContext(ctx, selectSQL, args...)
		if err != nil {
			return fmt.Errorf("select next jobs: %w", err)
		}
		var ids []any
		for rows.Next() {
			var (
				job     queue.Job
				payload string
			)
			if err := rows.Scan(&job.ID, &job.Kind, &job.Queue, &payload, &job.Attempt, &job.MaxAttempts, &job.Priority, &job.RunAt, &job.EnqueuedAt, &job.Key); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan next job: %w", err)
			}
			job.Attempt++
			job.RunAt, job.EnqueuedAt = job.RunAt.UTC(), job.EnqueuedAt.UTC()
			claimed = append(claimed, queue.Claimed{Job: job, Payload: []byte(payload), Lease: queue.Lease{JobID: job.ID, Token: token}})
			ids = append(ids, job.ID)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("select next jobs: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		// Exactly the rows this transaction locked; an UPDATE with the
		// eligibility predicate would wait on rows other claims hold.
		leaseArgs := append([]any{token, request.WorkerID, request.Lease.Microseconds()}, ids...)
		result, err := tx.ExecContext(ctx, `
			UPDATE forge_jobs
			SET state = 'running', attempts = attempts + 1, lease_token = ?, locked_by = ?,
			    lease_until = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? MICROSECOND)
			WHERE id IN `+inClause(len(ids)), leaseArgs...) // #nosec G202 -- inClause emits only "?" placeholders
		if err != nil {
			return fmt.Errorf("lease jobs: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("lease jobs: %w", err)
		}
		if n != int64(len(ids)) {
			return fmt.Errorf("lease jobs: %d of %d locked rows changed", n, len(ids))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	return claimed, nil
}

// Recover makes jobs whose lease expired pending again, or dead when they
// used their last attempt. It handles up to queue.RecoverBatch jobs per call.
// It runs at
// READ COMMITTED, like Claim: at REPEATABLE READ the scan of the
// (state, lease_until) index would take next-key locks on neighbouring rows
// and block the heartbeats and acknowledgements of live jobs.
func (s *Store) Recover(ctx context.Context) (int64, error) {
	var changed int64
	err := s.db.InTxWith(ctx, sqldb.TxOptions{Isolation: sql.LevelReadCommitted, Attempts: claimAttempts}, func(tx *sql.Tx) error {
		changed = 0
		// Lock only the expired rows, skipping any a heartbeat holds, and
		// then update exactly those: an UPDATE with the range predicate would
		// queue behind the lock on a neighbouring live job's index entry.
		rows, err := tx.QueryContext(ctx, `
			SELECT id FROM forge_jobs
			WHERE state = 'running' AND lease_until <= UTC_TIMESTAMP(6)
			ORDER BY lease_until
			LIMIT ?
			FOR UPDATE SKIP LOCKED`, queue.RecoverBatch)
		if err != nil {
			return err
		}
		var ids []any
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE forge_jobs
			SET state = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END,
			    finished_at = CASE WHEN attempts >= max_attempts THEN UTC_TIMESTAMP(6) END,
			    lease_token = NULL, lease_until = NULL, locked_by = NULL,
			    last_error = 'lease expired: the worker stopped renewing it'
			WHERE id IN `+inClause(len(ids)), ids...) // #nosec G202 -- inClause emits only "?" placeholders
		if err != nil {
			return err
		}
		changed, err = result.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("recover expired job leases: %w", err)
	}
	return changed, nil
}

// fenced runs an UPDATE guarded by id and lease token; no changed row means
// the lease is gone.
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

// Renew extends the lease of a running job. MySQL reports only changed rows,
// so a renewal that finds the same timestamp is confirmed with a read.
func (s *Store) Renew(ctx context.Context, lease queue.Lease, extend time.Duration) error {
	err := s.fenced(ctx, "renew job lease", `
		UPDATE forge_jobs
		SET lease_until = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? MICROSECOND)
		WHERE id = ? AND lease_token = ? AND state = 'running'
	`, extend.Microseconds(), lease.JobID, lease.Token)
	if !errors.Is(err, queue.ErrLeaseLost) {
		return err
	}
	var held int
	readErr := s.db.QueryRowContext(ctx, "SELECT 1 FROM forge_jobs WHERE id = ? AND lease_token = ? AND state = 'running'", lease.JobID, lease.Token).Scan(&held)
	switch {
	case readErr == nil:
		return nil
	case errors.Is(readErr, sql.ErrNoRows):
		return queue.ErrLeaseLost
	default:
		return fmt.Errorf("confirm job lease: %w", readErr)
	}
}

// Complete marks the job succeeded.
func (s *Store) Complete(ctx context.Context, lease queue.Lease) error {
	return s.fenced(ctx, "complete job", `
		UPDATE forge_jobs
		SET state = 'succeeded', finished_at = UTC_TIMESTAMP(6),
		    lease_token = NULL, lease_until = NULL, locked_by = NULL
		WHERE id = ? AND lease_token = ? AND state = 'running'
	`, lease.JobID, lease.Token)
}

// Fail schedules a retry, or kills the job when failure.Dead.
func (s *Store) Fail(ctx context.Context, lease queue.Lease, failure queue.Failure) error {
	if failure.Dead {
		return s.fenced(ctx, "fail job", `
			UPDATE forge_jobs
			SET state = 'dead', finished_at = UTC_TIMESTAMP(6), last_error = ?,
			    lease_token = NULL, lease_until = NULL, locked_by = NULL
			WHERE id = ? AND lease_token = ? AND state = 'running'
		`, failure.Message, lease.JobID, lease.Token)
	}
	return s.fenced(ctx, "schedule job retry", `
		UPDATE forge_jobs
		SET state = 'pending', run_at = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? MICROSECOND), last_error = ?,
		    lease_token = NULL, lease_until = NULL, locked_by = NULL
		WHERE id = ? AND lease_token = ? AND state = 'running'
	`, failure.RetryIn.Microseconds(), failure.Message, lease.JobID, lease.Token)
}

// Release returns the job to the queue without counting the attempt.
func (s *Store) Release(ctx context.Context, lease queue.Lease) error {
	return s.fenced(ctx, "release job", `
		UPDATE forge_jobs
		SET state = 'pending', attempts = GREATEST(attempts - 1, 0),
		    lease_token = NULL, lease_until = NULL, locked_by = NULL
		WHERE id = ? AND lease_token = ? AND state = 'running'
	`, lease.JobID, lease.Token)
}

// Stats returns per-queue counts using the database clock.
func (s *Store) Stats(ctx context.Context) (queue.Stats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT queue,
		       COUNT(CASE WHEN state = 'pending' AND run_at <= UTC_TIMESTAMP(6) THEN 1 END),
		       COUNT(CASE WHEN state = 'pending' AND run_at > UTC_TIMESTAMP(6) THEN 1 END),
		       COUNT(CASE WHEN state = 'running' THEN 1 END),
		       COUNT(CASE WHEN state = 'dead' THEN 1 END),
		       COUNT(CASE WHEN state = 'succeeded' THEN 1 END),
		       MIN(CASE WHEN state = 'pending' AND run_at <= UTC_TIMESTAMP(6) THEN run_at END)
		FROM forge_jobs
		GROUP BY queue
		ORDER BY queue`)
	if err != nil {
		return queue.Stats{}, fmt.Errorf("read job queue stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var stats queue.Stats
	for rows.Next() {
		var q queue.QueueStats
		var oldest sql.NullTime
		if err := rows.Scan(&q.Queue, &q.Ready, &q.Scheduled, &q.Running, &q.Dead, &q.Succeeded, &oldest); err != nil {
			return queue.Stats{}, fmt.Errorf("scan job queue stats: %w", err)
		}
		if oldest.Valid {
			value := oldest.Time.UTC()
			q.OldestReadyAt = &value
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
	var state string
	var leaseUntil, finishedAt sql.NullTime
	err := row.Scan(&info.Job.ID, &info.Job.Kind, &info.Job.Queue, &state, &info.Job.Attempt, &info.Job.MaxAttempts,
		&info.Job.Priority, &info.Job.RunAt, &info.Job.EnqueuedAt, &info.Job.Key, &info.LastError, &info.WorkerID,
		&leaseUntil, &finishedAt)
	if err != nil {
		return queue.Info{}, err
	}
	info.State = queue.State(state)
	info.Job.RunAt, info.Job.EnqueuedAt = info.Job.RunAt.UTC(), info.Job.EnqueuedAt.UTC()
	if leaseUntil.Valid {
		value := leaseUntil.Time.UTC()
		info.LeaseUntil = &value
	}
	if finishedAt.Valid {
		value := finishedAt.Time.UTC()
		info.FinishedAt = &value
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
		after := query.After.FinishedAt.UTC()
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
		SET state = 'pending', attempts = 0, run_at = UTC_TIMESTAMP(6), finished_at = NULL
		WHERE id = ? AND state = 'dead'
	`, id)
	if err != nil {
		if forgemysql.ErrorNumber(err) == duplicateEntry {
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
		WHERE state = 'succeeded' AND finished_at < DATE_SUB(UTC_TIMESTAMP(6), INTERVAL ? MICROSECOND)
		ORDER BY finished_at
		LIMIT ?
	`, retention.Microseconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("prune finished jobs: %w", err)
	}
	return result.RowsAffected()
}
