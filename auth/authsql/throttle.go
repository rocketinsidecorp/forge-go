package authsql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// LoginThrottler is an auth.LoginThrottler that keeps its counters in the
// database, so every process of an application shares them.
type LoginThrottler struct {
	repository *Repository
	config     auth.LoginThrottleConfig
}

var _ auth.LoginThrottler = (*LoginThrottler)(nil)

// NewLoginThrottler returns a LoginThrottler on db for config, which must be
// valid.
func NewLoginThrottler(db *sqldb.DB, dialect *Dialect, config auth.LoginThrottleConfig) (*LoginThrottler, error) {
	if db == nil {
		return nil, errors.New("login throttle database is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	repository, err := New(db, dialect)
	if err != nil {
		return nil, err
	}
	return &LoginThrottler{repository: repository, config: config}, nil
}

// inlinePruneBatch is how many expired counters each Attempt deletes. One
// attempt creates at most three counters, so deleting more than that keeps
// the table at the size of the live windows without a scheduled job.
const inlinePruneBatch = 8

// countAttempt counts one attempt on the counters its WHERE (counterKeys)
// names, starting a counter over once its window has passed.
const countAttempt = `
	UPDATE forge_login_throttle
	SET
		attempt_count = CASE WHEN reset_at <= ? THEN 1 ELSE attempt_count + 1 END,
		reset_at = CASE WHEN reset_at <= ? THEN ? ELSE reset_at END
	WHERE `

// errThrottleRefused rolls an attempt's transaction back when it is refused.
var errThrottleRefused = errors.New("login throttle refused the attempt")

// counterKeys is the WHERE condition that matches n (scope, key_hash) pairs.
func counterKeys(n int) string {
	return "(" + strings.TrimSuffix(strings.Repeat("(scope = ? AND key_hash = ?) OR ", n), " OR ") + ")"
}

// Attempt counts one attempt against the account, source and pair counters
// of key in one transaction, or refuses it without counting it when any of
// them is exhausted. Counters are created empty first and then locked in a
// fixed order (scope, then key), so concurrent first attempts serialize and
// no limit is exceeded. Expired counters are pruned as a side effect.
func (l *LoginThrottler) Attempt(ctx context.Context, key auth.LoginThrottleKey) (auth.LoginThrottleDecision, error) {
	r := l.repository
	counters := l.config.Counters(key)
	keys := make([]any, 0, 2*len(counters))
	for _, counter := range counters {
		keys = append(keys, counter.Scope, counter.Digest[:])
	}
	decision := auth.LoginThrottleDecision{Allowed: true}
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		var stamp timestamp
		if err := tx.QueryRowContext(ctx, r.q(`SELECT {now}`)).Scan(&stamp); err != nil {
			return r.translate(err)
		}
		now := stamp.Time
		resetAt := r.dialect.Time(now.Add(l.config.Window))
		insert := make([]any, 0, 4*len(counters))
		for _, counter := range counters {
			insert = append(insert, counter.Scope, counter.Digest[:], 0, resetAt)
		}
		if _, err := tx.ExecContext(ctx, r.dialect.InsertIgnore("forge_login_throttle",
			[]string{"scope", "key_hash", "attempt_count", "reset_at"}, []string{"scope", "key_hash"}, len(counters)), insert...); err != nil {
			return r.translate(err)
		}
		rows, err := tx.QueryContext(ctx, r.q(`
			SELECT scope, attempt_count, reset_at
			FROM forge_login_throttle
			WHERE `+counterKeys(len(counters))+`
			ORDER BY scope, key_hash{lock}
		`), keys...)
		if err != nil {
			return r.translate(err)
		}
		defer func() { _ = rows.Close() }()
		type stored struct {
			count   int
			resetAt time.Time
		}
		held := make(map[int16]stored, len(counters))
		for rows.Next() {
			var (
				scope   int16
				current stored
				reset   timestamp
			)
			if err := rows.Scan(&scope, &current.count, &reset); err != nil {
				return r.translate(err)
			}
			current.resetAt = reset.Time
			held[scope] = current
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return r.translate(err)
		}
		var retryAfter time.Duration
		for _, counter := range counters {
			if current := held[counter.Scope]; current.resetAt.After(now) && current.count >= counter.Limit {
				retryAfter = max(retryAfter, current.resetAt.Sub(now))
			}
		}
		if retryAfter > 0 {
			// Count nothing, and leave nothing behind: roll the empty rows
			// back too.
			decision = auth.LoginThrottleDecision{Allowed: false, RetryAfter: retryAfter}
			return errThrottleRefused
		}
		nowArg := r.dialect.Time(now)
		args := append([]any{nowArg, nowArg, resetAt}, keys...)
		update := countAttempt + counterKeys(len(counters)) //nolint:gosec // G202: counterKeys writes placeholders only
		if _, err := tx.ExecContext(ctx, update, args...); err != nil {
			return r.translate(err)
		}
		_, err = l.prune(ctx, tx, inlinePruneBatch)
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

// prune deletes at most limit expired counters through tx, skipping the ones
// another transaction holds (MySQL), and reports how many.
func (l *LoginThrottler) prune(ctx context.Context, tx *sql.Tx, limit int) (int64, error) {
	r := l.repository
	rows, err := tx.QueryContext(ctx, r.q(`
		SELECT scope, key_hash
		FROM forge_login_throttle
		WHERE reset_at <= {now}
		ORDER BY reset_at
		LIMIT ?{skip_locked}
	`), limit)
	if err != nil {
		return 0, r.translate(err)
	}
	defer func() { _ = rows.Close() }()
	var keys []any
	for rows.Next() {
		var (
			scope int16
			hash  []byte
		)
		if err := rows.Scan(&scope, &hash); err != nil {
			return 0, r.translate(err)
		}
		keys = append(keys, scope, hash)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, r.translate(err)
	}
	if len(keys) == 0 {
		return 0, nil
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM forge_login_throttle WHERE `+counterKeys(len(keys)/2), keys...) //nolint:gosec // G202: counterKeys writes placeholders only
	return affected(result, err, r.translate)
}

// Success clears the pair and account counters of key, not its source
// counter.
func (l *LoginThrottler) Success(ctx context.Context, key auth.LoginThrottleKey) error {
	keys := []any{auth.AccountThrottleScope, key.Account[:]}
	if key.HasPair {
		keys = append(keys, auth.PairThrottleScope, key.Pair[:])
	}
	_, err := l.repository.db.ExecContext(ctx, `DELETE FROM forge_login_throttle WHERE `+counterKeys(len(keys)/2), keys...)
	return l.repository.translate(err)
}

// PruneExpired deletes at most limit expired throttle counters. Attempt
// already prunes a few on every call, so this is only needed to empty a
// table that stopped receiving attempts.
func (l *LoginThrottler) PruneExpired(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, errors.New("prune limit must be positive")
	}
	var total int64
	err := l.repository.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		total, err = l.prune(ctx, tx, limit)
		return err
	})
	return total, err
}
