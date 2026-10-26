package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/postgres"
)

// LoginThrottler is an auth.LoginThrottler that keeps its counters in PostgreSQL, so every replica
// of an application shares them.
type LoginThrottler struct {
	db     *postgres.DB
	config auth.LoginThrottleConfig
}

var _ auth.LoginThrottler = (*LoginThrottler)(nil)

// NewRecoveryThrottler returns a LoginThrottler with auth.DefaultRecoveryThrottleConfig.
func NewRecoveryThrottler(db *postgres.DB) (*LoginThrottler, error) {
	return NewLoginThrottler(db, auth.DefaultRecoveryThrottleConfig())
}

// NewLoginThrottler returns a LoginThrottler for config, which must be valid.
func NewLoginThrottler(db *postgres.DB, config auth.LoginThrottleConfig) (*LoginThrottler, error) {
	if db == nil {
		return nil, errors.New("login throttle database is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &LoginThrottler{db: db, config: config}, nil
}

// inlinePruneBatch is how many expired counters each Attempt deletes. One
// attempt creates at most three counters, so deleting more than that keeps
// the table at the size of the live windows however many different logins and
// sources an attacker cycles through, without a scheduled job.
const inlinePruneBatch = 8

// Attempt counts one attempt against the account, source and pair counters
// of key in one transaction, or refuses it without counting it when any of
// them is exhausted: an attacker who is already blocked cannot burn the
// account and source counters other people depend on. Counters are locked in
// a fixed order (scope, then key), so concurrent attempts neither deadlock
// nor exceed a limit. Expired counters are pruned as a side effect.
func (l *LoginThrottler) Attempt(ctx context.Context, key auth.LoginThrottleKey) (auth.LoginThrottleDecision, error) {
	counters := l.config.Counters(key)
	scopes := make([]int16, len(counters))
	digests := make([][]byte, len(counters))
	for i, counter := range counters {
		scopes[i], digests[i] = counter.Scope, counter.Digest[:]
	}

	decision := auth.LoginThrottleDecision{Allowed: true}
	err := l.db.InTx(ctx, func(tx pgx.Tx) error {
		var now time.Time
		if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			return postgres.Translate(err)
		}
		now = now.UTC()

		// Make sure every counter row exists, so the next statement locks
		// all of them: two first attempts for one key serialize here instead
		// of both seeing no counter and both being allowed.
		if _, err := tx.Exec(ctx, `
			INSERT INTO forge_login_throttle (scope, key_hash, attempt_count, reset_at)
			SELECT scope, key_hash, 0, $3
			FROM unnest($1::smallint[], $2::bytea[]) AS attempt(scope, key_hash)
			ORDER BY scope
			ON CONFLICT (scope, key_hash) DO NOTHING
		`, scopes, digests, now.Add(l.config.Window)); err != nil {
			return postgres.Translate(err)
		}
		rows, err := tx.Query(ctx, `
			SELECT scope, attempt_count, reset_at
			FROM forge_login_throttle
			WHERE (scope, key_hash) IN (SELECT * FROM unnest($1::smallint[], $2::bytea[]))
			ORDER BY scope, key_hash
			FOR UPDATE
		`, scopes, digests)
		if err != nil {
			return postgres.Translate(err)
		}
		type stored struct {
			count   int
			resetAt time.Time
		}
		held := make(map[int16]stored, len(counters))
		for rows.Next() {
			var scope int16
			var current stored
			if err := rows.Scan(&scope, &current.count, &current.resetAt); err != nil {
				rows.Close()
				return postgres.Translate(err)
			}
			held[scope] = current
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return postgres.Translate(err)
		}

		var retryAfter time.Duration
		for _, counter := range counters {
			if current := held[counter.Scope]; current.resetAt.After(now) && current.count >= counter.Limit {
				if retry := current.resetAt.Sub(now); retry > retryAfter {
					retryAfter = retry
				}
			}
		}
		if retryAfter > 0 {
			// Count nothing, and leave nothing behind: roll the empty rows
			// back too.
			decision = auth.LoginThrottleDecision{Allowed: false, RetryAfter: retryAfter}
			return errThrottleRefused
		}

		if _, err := tx.Exec(ctx, `
			UPDATE forge_login_throttle
			SET
				attempt_count = CASE WHEN reset_at <= $3 THEN 1 ELSE attempt_count + 1 END,
				reset_at = CASE WHEN reset_at <= $3 THEN $4 ELSE reset_at END
			WHERE (scope, key_hash) IN (SELECT * FROM unnest($1::smallint[], $2::bytea[]))
		`, scopes, digests, now, now.Add(l.config.Window)); err != nil {
			return postgres.Translate(err)
		}
		_, err = pruneExpired(ctx, tx, inlinePruneBatch)
		return err
	})
	if errors.Is(err, errThrottleRefused) {
		return decision, nil
	}
	if err != nil {
		return auth.LoginThrottleDecision{}, err
	}
	return decision, nil
}

// errThrottleRefused rolls an attempt's transaction back when it is refused.
var errThrottleRefused = errors.New("login throttle refused the attempt")

type executor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// pruneExpired deletes at most limit expired counters and reports how many.
// SKIP LOCKED keeps concurrent cleaners from blocking each other or the
// attempts that are resetting a counter.
func pruneExpired(ctx context.Context, db executor, limit int) (int64, error) {
	tag, err := db.Exec(ctx, `
		WITH doomed AS (
			SELECT scope, key_hash
			FROM forge_login_throttle
			WHERE reset_at <= clock_timestamp()
			ORDER BY reset_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM forge_login_throttle target
		USING doomed
		WHERE target.scope = doomed.scope
		  AND target.key_hash = doomed.key_hash
	`, limit)
	if err != nil {
		return 0, postgres.Translate(err)
	}
	return tag.RowsAffected(), nil
}

// Success clears the pair and account counters of key, not its source
// counter.
func (l *LoginThrottler) Success(ctx context.Context, key auth.LoginThrottleKey) error {
	scopes, digests := []int16{auth.AccountThrottleScope}, [][]byte{key.Account[:]}
	if key.HasPair {
		scopes, digests = append(scopes, auth.PairThrottleScope), append(digests, key.Pair[:])
	}
	_, err := l.db.Exec(ctx, `
		DELETE FROM forge_login_throttle
		WHERE (scope, key_hash) IN (SELECT * FROM unnest($1::smallint[], $2::bytea[]))
	`, scopes, digests)
	if err != nil {
		return postgres.Translate(err)
	}
	return nil
}

// PruneExpired deletes at most limit expired throttle counters. Attempt
// already prunes a few on every call, so this is only needed to empty a table
// that stopped receiving attempts. Multiple replicas may call it
// concurrently; SKIP LOCKED keeps cleanup workers from blocking each other.
func (l *LoginThrottler) PruneExpired(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, errors.New("prune limit must be positive")
	}
	return pruneExpired(ctx, l.db, limit)
}
