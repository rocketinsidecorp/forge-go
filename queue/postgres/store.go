// Package postgres is the PostgreSQL adapter of Forge's job queue (ADR 0023).
//
// Workers claim jobs with FOR UPDATE SKIP LOCKED in a single autocommit
// statement, several at a time (ClaimBatch), so no transaction or row lock is
// held while a handler runs, and all lease timestamps come from the database
// clock, so skew between application hosts cannot expire a lease early. Every
// acknowledgement is fenced by the lease token of the claim.
//
// Enqueue inside the caller's transaction with Store.In:
//
//	err := db.InTx(ctx, func(tx pgx.Tx) error {
//	    // ... business writes using tx ...
//	    _, err := queue.Enqueue(ctx, store.In(tx), registry, "invoice.send", SendInvoice{InvoiceID: id})
//	    return err
//	})
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	forgepostgres "github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// uniqueViolation is SQLSTATE 23505.
const uniqueViolation = "23505"

// Store is the PostgreSQL implementation of queue.Store.
type Store struct {
	pool *pgxpool.Pool
}

var (
	_ queue.Store        = (*Store)(nil)
	_ queue.BatchClaimer = (*Store)(nil)
)

// NewStore returns a store on db's pool. The tables come from Migrations.
func NewStore(db *forgepostgres.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("queue PostgreSQL database is required")
	}
	return NewStoreFromPool(db.Pool())
}

// NewStoreFromPool is NewStore for an application-owned pool.
func NewStoreFromPool(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("queue PostgreSQL pool is required")
	}
	return &Store{pool: pool}, nil
}

// executor is what both the pool and a pgx.Tx provide.
type executor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// In returns an Inserter that writes through tx, so the job commits or rolls
// back with the caller's transaction and is invisible to workers until then.
// No second transaction is opened and nothing is committed by the store.
//
// Under REPEATABLE READ or SERIALIZABLE, two transactions that enqueue the
// same key concurrently make one fail with SQLSTATE 40001, like any
// conflicting write; run them with postgres.TxOptions.Attempts so the retry
// sees the committed job and reports a duplicate. READ COMMITTED needs no retry.
func (s *Store) In(tx pgx.Tx) queue.Inserter { return inserter{exec: tx} }

type inserter struct{ exec executor }

// Insert stores job in its own implicit transaction (a single statement).
// Use In for atomicity with business data.
func (s *Store) Insert(ctx context.Context, job queue.NewJob) (queue.InsertResult, error) {
	return inserter{exec: s.pool}.Insert(ctx, job)
}

func (in inserter) Insert(ctx context.Context, job queue.NewJob) (queue.InsertResult, error) {
	if in.exec == nil {
		return queue.InsertResult{}, errors.New("queue transaction is required")
	}
	if err := job.Validate(); err != nil {
		return queue.InsertResult{}, err
	}
	var runAt any
	if !job.RunAt.IsZero() {
		runAt = job.RunAt.UTC()
	}
	// The conflict target is the partial unique index of active jobs. DO
	// NOTHING keeps a duplicate from aborting the caller's transaction.
	const insertSQL = `
		INSERT INTO forge_jobs (id, queue, kind, payload, priority, max_attempts, run_at, idempotency_key)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, COALESCE($7, clock_timestamp()), NULLIF($8::text, ''))
		ON CONFLICT (kind, idempotency_key) WHERE idempotency_key IS NOT NULL AND state IN ('pending', 'running') DO NOTHING
		RETURNING id`
	const existingSQL = `
		SELECT id FROM forge_jobs
		WHERE kind = $1 AND idempotency_key = $2 AND state IN ('pending', 'running')`
	for range 3 {
		var id uuid.UUID
		err := in.exec.QueryRow(ctx, insertSQL, job.ID, job.Queue, job.Kind, []byte(job.Payload), job.Priority, job.MaxAttempts, runAt, job.Key).Scan(&id)
		if err == nil {
			return queue.InsertResult{ID: id}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return queue.InsertResult{}, forgepostgres.Translate(fmt.Errorf("insert job: %w", err))
		}
		err = in.exec.QueryRow(ctx, existingSQL, job.Kind, job.Key).Scan(&id)
		if err == nil {
			return queue.InsertResult{ID: id, Duplicate: true}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return queue.InsertResult{}, forgepostgres.Translate(fmt.Errorf("find job holding idempotency key: %w", err))
		}
		// The holder finished between the two statements; insert again.
	}
	return queue.InsertResult{}, errors.New("queue: idempotency key conflict could not be resolved")
}

// Claim leases the next eligible job of the request.
func (s *Store) Claim(ctx context.Context, request queue.ClaimRequest) (queue.Claimed, bool, error) {
	claimed, err := s.ClaimBatch(ctx, request, 1)
	if err != nil || len(claimed) == 0 {
		return queue.Claimed{}, false, err
	}
	return claimed[0], true, nil
}

// ClaimBatch leases up to limit eligible jobs of the request in one
// statement. SKIP LOCKED makes concurrent claims take disjoint jobs; the jobs
// share one lease token.
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
	// MATERIALIZED keeps the locked candidate set fixed: the UPDATE joins
	// exactly the rows the CTE locked.
	rows, err := s.pool.Query(ctx, `
		WITH candidate AS MATERIALIZED (
			SELECT id FROM forge_jobs
			WHERE state = 'pending' AND queue = $1 AND kind = ANY($2)
			  AND run_at <= clock_timestamp()
			ORDER BY priority, run_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $6
		)
		UPDATE forge_jobs AS j
		SET state = 'running',
		    attempts = j.attempts + 1,
		    lease_token = $3,
		    lease_until = clock_timestamp() + ($4 * interval '1 microsecond'),
		    locked_by = $5
		FROM candidate
		WHERE j.id = candidate.id
		RETURNING j.id, j.kind, j.queue, j.payload, j.attempts, j.max_attempts, j.priority,
		          j.run_at, j.enqueued_at, COALESCE(j.idempotency_key, '')
	`, request.Queue, request.Kinds, token, request.Lease.Microseconds(), request.WorkerID, limit)
	if err != nil {
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	defer rows.Close()
	claimed := make([]queue.Claimed, 0, limit)
	for rows.Next() {
		var (
			job     queue.Job
			payload []byte
		)
		if err := rows.Scan(&job.ID, &job.Kind, &job.Queue, &payload, &job.Attempt, &job.MaxAttempts, &job.Priority,
			&job.RunAt, &job.EnqueuedAt, &job.Key); err != nil {
			return nil, fmt.Errorf("scan claimed job: %w", err)
		}
		job.RunAt, job.EnqueuedAt = job.RunAt.UTC(), job.EnqueuedAt.UTC()
		claimed = append(claimed, queue.Claimed{Job: job, Payload: payload, Lease: queue.Lease{JobID: job.ID, Token: token}})
	}
	if err := rows.Err(); err != nil {
		// The statement may have committed before the error: those leases
		// expire and Recover makes the jobs pending again.
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	return claimed, nil
}

// Recover makes jobs whose lease expired pending again, or dead when they
// used their last attempt. It handles up to queue.RecoverBatch jobs per call.
func (s *Store) Recover(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		WITH expired AS (
			SELECT id FROM forge_jobs
			WHERE state = 'running' AND lease_until <= clock_timestamp()
			ORDER BY lease_until
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE forge_jobs AS j
		SET state = CASE WHEN j.attempts >= j.max_attempts THEN 'dead' ELSE 'pending' END,
		    finished_at = CASE WHEN j.attempts >= j.max_attempts THEN clock_timestamp() END,
		    lease_token = NULL, lease_until = NULL, locked_by = NULL,
		    last_error = 'lease expired: the worker stopped renewing it'
		FROM expired
		WHERE j.id = expired.id
	`, queue.RecoverBatch)
	if err != nil {
		return 0, fmt.Errorf("recover expired job leases: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Renew extends the lease of a running job.
func (s *Store) Renew(ctx context.Context, lease queue.Lease, extend time.Duration) error {
	return s.fenced(ctx, "renew job lease", `
		UPDATE forge_jobs
		SET lease_until = clock_timestamp() + ($3 * interval '1 microsecond')
		WHERE id = $1 AND lease_token = $2 AND state = 'running'
	`, lease.JobID, lease.Token, extend.Microseconds())
}

// Complete marks the job succeeded.
func (s *Store) Complete(ctx context.Context, lease queue.Lease) error {
	return s.fenced(ctx, "complete job", `
		UPDATE forge_jobs
		SET state = 'succeeded', finished_at = clock_timestamp(),
		    lease_token = NULL, lease_until = NULL, locked_by = NULL
		WHERE id = $1 AND lease_token = $2 AND state = 'running'
	`, lease.JobID, lease.Token)
}

// Fail schedules a retry, or kills the job when failure.Dead.
func (s *Store) Fail(ctx context.Context, lease queue.Lease, failure queue.Failure) error {
	message := failure.Message
	if failure.Dead {
		return s.fenced(ctx, "fail job", `
			UPDATE forge_jobs
			SET state = 'dead', finished_at = clock_timestamp(), last_error = $3,
			    lease_token = NULL, lease_until = NULL, locked_by = NULL
			WHERE id = $1 AND lease_token = $2 AND state = 'running'
		`, lease.JobID, lease.Token, message)
	}
	return s.fenced(ctx, "schedule job retry", `
		UPDATE forge_jobs
		SET state = 'pending', run_at = clock_timestamp() + ($4 * interval '1 microsecond'), last_error = $3,
		    lease_token = NULL, lease_until = NULL, locked_by = NULL
		WHERE id = $1 AND lease_token = $2 AND state = 'running'
	`, lease.JobID, lease.Token, message, failure.RetryIn.Microseconds())
}

// Release returns the job to the queue without counting the attempt.
func (s *Store) Release(ctx context.Context, lease queue.Lease) error {
	return s.fenced(ctx, "release job", `
		UPDATE forge_jobs
		SET state = 'pending', attempts = GREATEST(attempts - 1, 0),
		    lease_token = NULL, lease_until = NULL, locked_by = NULL
		WHERE id = $1 AND lease_token = $2 AND state = 'running'
	`, lease.JobID, lease.Token)
}

// fenced runs an UPDATE guarded by id and lease token; no row means the lease
// is gone.
func (s *Store) fenced(ctx context.Context, what, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return nil
}

// Stats returns per-queue counts using the database clock.
func (s *Store) Stats(ctx context.Context) (queue.Stats, error) {
	rows, err := s.pool.Query(ctx, `
		WITH probe AS (SELECT clock_timestamp() AS now)
		SELECT queue,
		       count(*) FILTER (WHERE state = 'pending' AND run_at <= probe.now),
		       count(*) FILTER (WHERE state = 'pending' AND run_at > probe.now),
		       count(*) FILTER (WHERE state = 'running'),
		       count(*) FILTER (WHERE state = 'dead'),
		       count(*) FILTER (WHERE state = 'succeeded'),
		       min(run_at) FILTER (WHERE state = 'pending' AND run_at <= probe.now)
		FROM forge_jobs, probe
		GROUP BY queue
		ORDER BY queue
	`)
	if err != nil {
		return queue.Stats{}, fmt.Errorf("read job queue stats: %w", err)
	}
	defer rows.Close()
	var stats queue.Stats
	for rows.Next() {
		var q queue.QueueStats
		var oldest pgtype.Timestamptz
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
	var leaseUntil, finishedAt pgtype.Timestamptz
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
	info, err := scanInfo(s.pool.QueryRow(ctx, "SELECT "+infoColumns+" FROM forge_jobs WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
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
	sql := "SELECT " + infoColumns + " FROM forge_jobs WHERE state = 'dead'"
	var args []any
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if query.Queue != "" {
		sql += " AND queue = " + arg(query.Queue)
	}
	if query.Kind != "" {
		sql += " AND kind = " + arg(query.Kind)
	}
	if query.After != nil {
		sql += " AND (finished_at, id) > (" + arg(query.After.FinishedAt.UTC()) + ", " + arg(query.After.ID) + ")"
	}
	sql += " ORDER BY finished_at, id LIMIT " + arg(limit+1)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return queue.DeadPage{}, fmt.Errorf("list dead jobs: %w", err)
	}
	defer rows.Close()
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
	tag, err := s.pool.Exec(ctx, `
		UPDATE forge_jobs
		SET state = 'pending', attempts = 0, run_at = clock_timestamp(), finished_at = NULL
		WHERE id = $1 AND state = 'dead'
	`, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return queue.ErrKeyInUse
		}
		return fmt.Errorf("retry dead job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrNotFound
	}
	return nil
}

// Discard permanently deletes a dead job.
func (s *Store) Discard(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM forge_jobs WHERE id = $1 AND state = 'dead'", id)
	if err != nil {
		return fmt.Errorf("discard dead job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrNotFound
	}
	return nil
}

// Prune deletes up to limit succeeded jobs finished before now-retention.
func (s *Store) Prune(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	if retention < 0 || limit < 1 {
		return 0, errors.New("prune needs a non-negative retention and a positive limit")
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM forge_jobs
		WHERE id IN (
			SELECT id FROM forge_jobs
			WHERE state = 'succeeded' AND finished_at < clock_timestamp() - ($1 * interval '1 microsecond')
			ORDER BY finished_at
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
	`, retention.Microseconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("prune finished jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}
