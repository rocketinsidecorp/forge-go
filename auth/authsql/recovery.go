package authsql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var (
	// ErrRecoveryIdentityRequired is returned when the account a recovery
	// token is created for is missing or not active.
	ErrRecoveryIdentityRequired = errors.New("active recovery identity is required")

	_ auth.RecoveryStore        = (*Repository)(nil)
	_ auth.RecoveryTokenChecker = (*Repository)(nil)
)

// LookupRecovery returns the active account with the normalized email that
// has an active membership in the tenant with slug tenantSlug. found is false
// otherwise.
func (r *Repository) LookupRecovery(ctx context.Context, emailNormalized, tenantSlug string) (auth.RecoveryIdentity, bool, error) {
	var identity auth.RecoveryIdentity
	err := r.db.QueryRowContext(ctx, r.q(`
		SELECT u.id, u.email
		FROM forge_users u
		JOIN forge_memberships m ON m.user_id = u.id
		JOIN forge_tenants t ON t.id = m.tenant_id
		JOIN forge_organizations o ON o.id = t.organization_id
		WHERE u.email_normalized = ?
		  AND t.slug = ?`+activeChain), emailNormalized, tenantSlug).Scan(&identity.SubjectID, &identity.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.RecoveryIdentity{}, false, nil
	}
	if err != nil {
		return auth.RecoveryIdentity{}, false, r.translate(err)
	}
	return identity, true, nil
}

// RecoveryTokenUsable reports whether digest names a recovery that is unused,
// unexpired and belongs to an active user. It reads without locking; the
// authoritative check is still ConsumeRecovery's.
func (r *Repository) RecoveryTokenUsable(ctx context.Context, digest auth.Digest) (bool, error) {
	var usable bool
	err := r.db.QueryRowContext(ctx, r.q(`
		SELECT EXISTS (
			SELECT 1
			FROM forge_password_recovery r
			JOIN forge_users u ON u.id = r.user_id
			WHERE r.token_digest = ?
			  AND r.consumed_at IS NULL
			  AND r.expires_at > {now}
			  AND u.status = 'active'
		)
	`), digest[:]).Scan(&usable)
	if err != nil {
		return false, r.translate(err)
	}
	return usable, nil
}

// CreateRecovery stores the digest of a recovery token that expires at
// expiresAt. Earlier unused tokens of the account are superseded in the same
// transaction.
func (r *Repository) CreateRecovery(ctx context.Context, subjectID uuid.UUID, digest auth.Digest, expiresAt time.Time) error {
	if !isV7(subjectID) {
		return errors.New("user ID must be a UUIDv7")
	}
	if !expiresAt.After(r.now()) {
		return errors.New("recovery expiry must be in the future")
	}
	return r.inTx(ctx, func(tx *sql.Tx) error {
		locked, err := r.lockUser(ctx, tx, subjectID, true)
		if err != nil {
			return err
		}
		if !locked {
			return ErrRecoveryIdentityRequired
		}
		if err := r.supersedeRecoveries(ctx, tx, subjectID); err != nil {
			return err
		}
		id, err := uuid.New()
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, r.q(`
			INSERT INTO forge_password_recovery (id, token_digest, user_id, expires_at, created_at)
			VALUES (?, ?, ?, ?, {now})
		`), id, digest[:], subjectID, r.dialect.Time(expiresAt))
		return r.translate(err)
	})
}

// supersedeRecoveries marks every unused recovery of subjectID used.
func (r *Repository) supersedeRecoveries(ctx context.Context, tx *sql.Tx, subjectID uuid.UUID) error {
	_, err := tx.ExecContext(ctx, r.q(`
		UPDATE forge_password_recovery
		SET consumed_at = {now}
		WHERE user_id = ? AND consumed_at IS NULL
	`), subjectID)
	return r.translate(err)
}

// lockUser locks the forge_users row of subjectID (active only when
// activeOnly); false when there is none.
func (r *Repository) lockUser(ctx context.Context, tx *sql.Tx, subjectID uuid.UUID, activeOnly bool) (bool, error) {
	query := `SELECT id FROM forge_users WHERE id = ?`
	if activeOnly {
		query += ` AND status = 'active'`
	}
	var locked uuid.UUID
	err := tx.QueryRowContext(ctx, r.q(query+`{lock}`), subjectID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, r.translate(err)
	}
	return true, nil
}

// userOf returns the user ID query (one row, one column, no locks) names;
// false when it names none.
func (r *Repository) userOf(ctx context.Context, tx *sql.Tx, query string, args ...any) (uuid.UUID, bool, error) {
	var subjectID uuid.UUID
	err := tx.QueryRowContext(ctx, r.q(query), args...).Scan(&subjectID)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.UUID{}, false, nil
	}
	if err != nil {
		return uuid.UUID{}, false, r.translate(err)
	}
	return subjectID, true, nil
}

// InvalidateRecovery marks the token with digest used without changing the
// password; it does nothing for an unknown token and keeps the time of an
// already used one.
func (r *Repository) InvalidateRecovery(ctx context.Context, digest auth.Digest) error {
	return r.inTx(ctx, func(tx *sql.Tx) error {
		subjectID, found, err := r.userOf(ctx, tx, `SELECT user_id FROM forge_password_recovery WHERE token_digest = ?`, digest[:])
		if err != nil || !found {
			return err
		}
		if locked, err := r.lockUser(ctx, tx, subjectID, false); err != nil || !locked {
			return err
		}
		_, err = tx.ExecContext(ctx, r.q(`
			UPDATE forge_password_recovery
			SET consumed_at = {now}
			WHERE token_digest = ? AND consumed_at IS NULL
		`), digest[:])
		return r.translate(err)
	})
}

// ConsumeRecovery uses the unexpired, unused token with digest, replaces the
// account's password hash with nextPasswordHash and revokes its sessions, all
// in one transaction. It reports false when the token does not qualify.
func (r *Repository) ConsumeRecovery(ctx context.Context, digest auth.Digest, nextPasswordHash string) (bool, error) {
	if nextPasswordHash == "" {
		return false, errors.New("next password hash is required")
	}
	consumed := false
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		subjectID, found, err := r.userOf(ctx, tx, `
			SELECT r.user_id
			FROM forge_password_recovery r
			JOIN forge_users u ON u.id = r.user_id
			WHERE r.token_digest = ?
			  AND r.consumed_at IS NULL
			  AND r.expires_at > {now}
			  AND u.status = 'active'
		`, digest[:])
		if err != nil || !found {
			return err
		}
		if locked, err := r.lockUser(ctx, tx, subjectID, true); err != nil || !locked {
			return err
		}
		var stillValid bool
		if err := tx.QueryRowContext(ctx, r.q(`
			SELECT EXISTS (
				SELECT 1
				FROM forge_password_recovery
				WHERE token_digest = ?
				  AND user_id = ?
				  AND consumed_at IS NULL
				  AND expires_at > {now}
			)
		`), digest[:], subjectID).Scan(&stillValid); err != nil {
			return r.translate(err)
		}
		if !stillValid {
			return nil
		}
		result, err := tx.ExecContext(ctx, r.q(`
			UPDATE forge_users
			SET password_hash = ?,
			    session_version = session_version + 1,
			    updated_at = {now}
			WHERE id = ? AND status = 'active'
		`), nextPasswordHash, subjectID)
		updated, err := affectedOne(result, err, r.translate)
		if err != nil {
			return err
		}
		if !updated {
			return errors.New("active recovery user disappeared while locked")
		}
		if err := r.revokeSessionsOf(ctx, tx, subjectID); err != nil {
			return err
		}
		if err := r.supersedeRecoveries(ctx, tx, subjectID); err != nil {
			return err
		}
		consumed = true
		return nil
	})
	return consumed, err
}

// PruneRecovery deletes at most limit consumed or expired recovery rows.
// Rows another transaction holds are skipped (MySQL), so several cleanup
// workers never block each other.
func (r *Repository) PruneRecovery(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, errors.New("prune limit must be positive")
	}
	return r.pruneByID(ctx, `
		SELECT id
		FROM forge_password_recovery
		WHERE consumed_at IS NOT NULL
		   OR expires_at <= {now}
		ORDER BY COALESCE(consumed_at, expires_at)
		LIMIT ?{skip_locked}
	`, `DELETE FROM forge_password_recovery WHERE id = ?`, limit)
}

// pruneByID selects at most limit keys with selectKeys (one column, locked
// with {skip_locked}) and deletes each with deleteKey, in one transaction.
func (r *Repository) pruneByID(ctx context.Context, selectKeys, deleteKey string, limit int) (int64, error) {
	var total int64
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		total = 0
		rows, err := tx.QueryContext(ctx, r.q(selectKeys), limit)
		if err != nil {
			return r.translate(err)
		}
		defer func() { _ = rows.Close() }()
		var keys []string
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return r.translate(err)
			}
			keys = append(keys, key)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return r.translate(err)
		}
		for _, key := range keys {
			result, err := tx.ExecContext(ctx, r.q(deleteKey), key)
			count, err := affected(result, err, r.translate)
			if err != nil {
				return err
			}
			total += count
		}
		return nil
	})
	return total, err
}
