package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/postgres"
)

type LoginThrottler struct {
	db     *postgres.DB
	config auth.LoginThrottleConfig
}

var _ auth.LoginThrottler = (*LoginThrottler)(nil)

func NewRecoveryThrottler(db *postgres.DB) (*LoginThrottler, error) {
	return NewLoginThrottler(db, auth.DefaultRecoveryThrottleConfig())
}

func NewLoginThrottler(db *postgres.DB, config auth.LoginThrottleConfig) (*LoginThrottler, error) {
	if db == nil {
		return nil, errors.New("login throttle database is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &LoginThrottler{db: db, config: config}, nil
}

func (l *LoginThrottler) Attempt(ctx context.Context, key auth.LoginThrottleKey) (auth.LoginThrottleDecision, error) {
	candidates := []struct {
		scope  int16
		digest [32]byte
		limit  int
	}{
		{scope: 1, digest: key.Account, limit: l.config.AccountLimit},
	}
	if key.HasSource {
		candidates = append(candidates, struct {
			scope  int16
			digest [32]byte
			limit  int
		}{scope: 2, digest: key.Source, limit: l.config.SourceLimit})
	}

	decision := auth.LoginThrottleDecision{Allowed: true}
	err := l.db.InTx(ctx, func(tx pgx.Tx) error {
		var now time.Time
		if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			return postgres.Translate(err)
		}
		now = now.UTC()
		resetAt := now.Add(l.config.Window)

		var retryAfter time.Duration
		for _, candidate := range candidates {
			var count int
			var candidateReset time.Time
			err := tx.QueryRow(ctx, `
				INSERT INTO forge_login_throttle (scope, key_hash, attempt_count, reset_at)
				VALUES ($1, $2, 1, $3)
				ON CONFLICT (scope, key_hash) DO UPDATE
				SET
					attempt_count = CASE
						WHEN forge_login_throttle.reset_at <= $4 THEN 1
						ELSE forge_login_throttle.attempt_count + 1
					END,
					reset_at = CASE
						WHEN forge_login_throttle.reset_at <= $4 THEN EXCLUDED.reset_at
						ELSE forge_login_throttle.reset_at
					END
				RETURNING attempt_count, reset_at
			`, candidate.scope, candidate.digest[:], resetAt, now).Scan(&count, &candidateReset)
			if err != nil {
				return postgres.Translate(err)
			}
			if count > candidate.limit {
				retry := candidateReset.Sub(now)
				if retry > retryAfter {
					retryAfter = retry
				}
			}
		}
		if retryAfter > 0 {
			decision = auth.LoginThrottleDecision{Allowed: false, RetryAfter: retryAfter}
		}
		return nil
	})
	if err != nil {
		return auth.LoginThrottleDecision{}, err
	}
	return decision, nil
}

func (l *LoginThrottler) Success(ctx context.Context, key auth.LoginThrottleKey) error {
	_, err := l.db.Exec(ctx, `
		DELETE FROM forge_login_throttle
		WHERE scope = 1 AND key_hash = $1
	`, key.Account[:])
	if err != nil {
		return postgres.Translate(err)
	}
	return nil
}

// PruneExpired deletes at most limit expired throttle buckets. Multiple
// replicas may call it concurrently; SKIP LOCKED keeps cleanup workers from
// blocking each other.
func (l *LoginThrottler) PruneExpired(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, errors.New("prune limit must be positive")
	}
	tag, err := l.db.Exec(ctx, `
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
