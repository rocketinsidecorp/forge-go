package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var (
	ErrRecoveryIdentityRequired                           = errors.New("active recovery identity is required")
	_                           auth.RecoveryStore        = (*Repository)(nil)
	_                           auth.RecoveryTokenChecker = (*Repository)(nil)
)

func (r *Repository) LookupRecovery(ctx context.Context, emailNormalized, tenantSlug string) (auth.RecoveryIdentity, bool, error) {
	var identity auth.RecoveryIdentity
	err := r.db.QueryRow(ctx, `
		SELECT u.id, u.email
		FROM forge_users u
		JOIN forge_memberships m ON m.user_id = u.id
		JOIN forge_tenants t ON t.id = m.tenant_id
		JOIN forge_organizations o ON o.id = t.organization_id
		WHERE u.email_normalized = $1
		  AND t.slug = $2
		  AND u.status = 'active'
		  AND m.status = 'active'
		  AND t.status = 'active'
		  AND o.status = 'active'
	`, emailNormalized, tenantSlug).Scan(&identity.SubjectID, &identity.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.RecoveryIdentity{}, false, nil
	}
	if err != nil {
		return auth.RecoveryIdentity{}, false, postgres.Translate(err)
	}
	return identity, true, nil
}

// RecoveryTokenUsable reports whether digest names a recovery that is unused,
// unexpired and belongs to an active user. It reads without locking; the
// authoritative check is still ConsumeRecovery's.
func (r *Repository) RecoveryTokenUsable(ctx context.Context, digest auth.Digest) (bool, error) {
	var usable bool
	err := r.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM forge_password_recovery r
			JOIN forge_users u ON u.id = r.user_id
			WHERE r.token_digest = $1
			  AND r.consumed_at IS NULL
			  AND r.expires_at > now()
			  AND u.status = 'active'
		)
	`, digest[:]).Scan(&usable)
	if err != nil {
		return false, postgres.Translate(err)
	}
	return usable, nil
}

func (r *Repository) CreateRecovery(ctx context.Context, subjectID uuid.UUID, digest auth.Digest, expiresAt time.Time) error {
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return errors.New("user ID must be a UUIDv7")
	}
	if !expiresAt.After(r.now()) {
		return errors.New("recovery expiry must be in the future")
	}
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var locked uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT id
			FROM forge_users
			WHERE id = $1 AND status = 'active'
			FOR UPDATE
		`, subjectID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRecoveryIdentityRequired
		}
		if err != nil {
			return postgres.Translate(err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE forge_password_recovery
			SET consumed_at = COALESCE(consumed_at, now())
			WHERE user_id = $1 AND consumed_at IS NULL
		`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO forge_password_recovery (token_digest, user_id, expires_at)
			VALUES ($1, $2, $3)
		`, digest[:], subjectID, expiresAt.UTC())
		return postgres.Translate(err)
	})
}

func (r *Repository) InvalidateRecovery(ctx context.Context, digest auth.Digest) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var subjectID uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT user_id
			FROM forge_password_recovery
			WHERE token_digest = $1
		`, digest[:]).Scan(&subjectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return postgres.Translate(err)
		}

		var locked uuid.UUID
		err = tx.QueryRow(ctx, `
			SELECT id
			FROM forge_users
			WHERE id = $1
			FOR UPDATE
		`, subjectID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return postgres.Translate(err)
		}

		_, err = tx.Exec(ctx, `
			UPDATE forge_password_recovery
			SET consumed_at = COALESCE(consumed_at, now())
			WHERE token_digest = $1 AND consumed_at IS NULL
		`, digest[:])
		return postgres.Translate(err)
	})
}

func (r *Repository) ConsumeRecovery(ctx context.Context, digest auth.Digest, nextPasswordHash string) (bool, error) {
	if nextPasswordHash == "" {
		return false, errors.New("next password hash is required")
	}
	consumed := false
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var subjectID uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT r.user_id
			FROM forge_password_recovery r
			JOIN forge_users u ON u.id = r.user_id
			WHERE r.token_digest = $1
			  AND r.consumed_at IS NULL
			  AND r.expires_at > now()
			  AND u.status = 'active'
		`, digest[:]).Scan(&subjectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return postgres.Translate(err)
		}

		var locked uuid.UUID
		err = tx.QueryRow(ctx, `
			SELECT id
			FROM forge_users
			WHERE id = $1 AND status = 'active'
			FOR UPDATE
		`, subjectID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return postgres.Translate(err)
		}

		var stillValid bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM forge_password_recovery
				WHERE token_digest = $1
				  AND user_id = $2
				  AND consumed_at IS NULL
				  AND expires_at > now()
			)
		`, digest[:], subjectID).Scan(&stillValid); err != nil {
			return postgres.Translate(err)
		}
		if !stillValid {
			return nil
		}

		tag, err := tx.Exec(ctx, `
			UPDATE forge_users
			SET password_hash = $2,
			    session_version = session_version + 1,
			    updated_at = now()
			WHERE id = $1 AND status = 'active'
		`, subjectID, nextPasswordHash)
		if err != nil {
			return postgres.Translate(err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("active recovery user disappeared while locked")
		}

		if _, err := tx.Exec(ctx, `
			UPDATE forge_sessions s
			SET revoked_at = COALESCE(s.revoked_at, now())
			FROM forge_memberships m
			WHERE s.membership_id = m.id
			  AND m.user_id = $1
			  AND s.revoked_at IS NULL
		`, subjectID); err != nil {
			return postgres.Translate(err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE forge_password_recovery
			SET consumed_at = COALESCE(consumed_at, now())
			WHERE user_id = $1 AND consumed_at IS NULL
		`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		consumed = true
		return nil
	})
	return consumed, err
}

// PruneRecovery deletes at most limit consumed or expired recovery rows.
// SKIP LOCKED allows multiple cleanup workers to run without blocking.
func (r *Repository) PruneRecovery(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, errors.New("prune limit must be positive")
	}
	tag, err := r.db.Exec(ctx, `
		WITH doomed AS (
			SELECT id
			FROM forge_password_recovery
			WHERE consumed_at IS NOT NULL
			   OR expires_at <= clock_timestamp()
			ORDER BY COALESCE(consumed_at, expires_at)
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM forge_password_recovery target
		USING doomed
		WHERE target.id = doomed.id
	`, limit)
	if err != nil {
		return 0, postgres.Translate(err)
	}
	return tag.RowsAffected(), nil
}
