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
	ErrMFAIdentityRequired               = errors.New("active MFA identity is required")
	_                      auth.MFAStore = (*Repository)(nil)
)

func (r *Repository) BeginTOTPEnrollment(
	ctx context.Context,
	subjectID uuid.UUID,
	secretDigest auth.Digest,
	secretCiphertext []byte,
	expiresAt time.Time,
) error {
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return errors.New("MFA subject ID must be a UUIDv7")
	}
	if len(secretCiphertext) == 0 {
		return errors.New("encrypted MFA secret is required")
	}
	if !expiresAt.After(r.now()) {
		return errors.New("MFA enrollment expiry must be in the future")
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
			return ErrMFAIdentityRequired
		}
		if err != nil {
			return postgres.Translate(err)
		}
		var alreadyEnabled bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM forge_totp_factors WHERE user_id = $1
			)
		`, subjectID).Scan(&alreadyEnabled); err != nil {
			return postgres.Translate(err)
		}
		if alreadyEnabled {
			return auth.ErrMFAAlreadyEnabled
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO forge_totp_enrollments
				(user_id, secret_digest, secret_ciphertext, expires_at, created_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (user_id) DO UPDATE
			SET secret_digest = EXCLUDED.secret_digest,
			    secret_ciphertext = EXCLUDED.secret_ciphertext,
			    expires_at = EXCLUDED.expires_at,
			    created_at = now()
		`, subjectID, secretDigest[:], secretCiphertext, expiresAt.UTC())
		return postgres.Translate(err)
	})
}

func (r *Repository) LoadPendingTOTP(ctx context.Context, subjectID uuid.UUID) (auth.TOTPSecretRecord, bool, error) {
	var (
		record    auth.TOTPSecretRecord
		rawDigest []byte
	)
	err := r.db.QueryRow(ctx, `
		SELECT e.user_id, e.secret_digest, e.secret_ciphertext, e.expires_at
		FROM forge_totp_enrollments e
		JOIN forge_users u ON u.id = e.user_id
		WHERE e.user_id = $1
		  AND u.status = 'active'
	`, subjectID).Scan(&record.SubjectID, &rawDigest, &record.SecretCiphertext, &record.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.TOTPSecretRecord{}, false, nil
	}
	if err != nil {
		return auth.TOTPSecretRecord{}, false, postgres.Translate(err)
	}
	digest, err := authDigest(rawDigest)
	if err != nil {
		return auth.TOTPSecretRecord{}, false, err
	}
	record.SecretDigest = digest
	record.ExpiresAt = record.ExpiresAt.UTC()
	return record, true, nil
}

func (r *Repository) ConfirmTOTPEnrollment(
	ctx context.Context,
	subjectID uuid.UUID,
	secretDigest auth.Digest,
	counter int64,
	backupCodeDigests []auth.Digest,
) (bool, error) {
	if counter < 0 {
		return false, errors.New("TOTP counter must be non-negative")
	}
	if len(backupCodeDigests) != auth.DefaultMFABackupCodeCount {
		return false, errors.New("TOTP confirmation requires the default MFA backup code set")
	}
	confirmed := false
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var (
			ciphertext []byte
			rawDigest  []byte
		)
		err := tx.QueryRow(ctx, `
			SELECT e.secret_ciphertext, e.secret_digest
			FROM forge_totp_enrollments e
			JOIN forge_users u ON u.id = e.user_id
			WHERE e.user_id = $1
			  AND e.secret_digest = $2
			  AND e.expires_at > now()
			  AND u.status = 'active'
			FOR UPDATE OF e, u
		`, subjectID, secretDigest[:]).Scan(&ciphertext, &rawDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return postgres.Translate(err)
		}
		storedDigest, err := authDigest(rawDigest)
		if err != nil {
			return err
		}
		if storedDigest != secretDigest {
			return errors.New("MFA enrollment digest changed while locked")
		}
		// The pending key of a rotation sits in the same table as an
		// enrollment's. With the user row locked, an account that has a
		// factor is refused here, explicitly, rather than by the INSERT
		// below failing on the factor's primary key.
		var noFactor bool
		if err := tx.QueryRow(ctx, `
			SELECT NOT EXISTS (
				SELECT 1 FROM forge_totp_factors WHERE user_id = $1
			)
		`, subjectID).Scan(&noFactor); err != nil {
			return postgres.Translate(err)
		}
		if !noFactor {
			return auth.ErrMFAAlreadyEnabled
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO forge_totp_factors
				(user_id, secret_digest, secret_ciphertext, last_counter, enabled_at, updated_at)
			VALUES ($1, $2, $3, $4, now(), now())
		`, subjectID, secretDigest[:], ciphertext, counter)
		if err != nil {
			return postgres.Translate(err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("TOTP factor was not created while enrollment was locked")
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM forge_mfa_backup_codes
			WHERE user_id = $1
		`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		for _, backupDigest := range backupCodeDigests {
			if _, err := tx.Exec(ctx, `
				INSERT INTO forge_mfa_backup_codes (user_id, code_digest)
				VALUES ($1, $2)
			`, subjectID, backupDigest[:]); err != nil {
				return postgres.Translate(err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE forge_users
			SET session_version = session_version + 1, updated_at = now()
			WHERE id = $1 AND status = 'active'
		`, subjectID); err != nil {
			return postgres.Translate(err)
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
			UPDATE forge_mfa_challenges c
			SET consumed_at = COALESCE(c.consumed_at, now())
			FROM forge_memberships m
			WHERE c.membership_id = m.id
			  AND m.user_id = $1
			  AND c.consumed_at IS NULL
		`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM forge_totp_enrollments
			WHERE user_id = $1
		`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		confirmed = true
		return nil
	})
	return confirmed, err
}

func (r *Repository) CreateMFAChallenge(
	ctx context.Context,
	digest auth.Digest,
	membershipID uuid.UUID,
	credentialVersion int64,
	expiresAt, sessionExpiresAt time.Time,
) error {
	if membershipID.Version() != 7 || membershipID.Variant() != 2 {
		return errors.New("MFA membership ID must be a UUIDv7")
	}
	if credentialVersion < 1 {
		return errors.New("MFA credential version must be positive")
	}
	now := r.now()
	if !expiresAt.After(now) || !sessionExpiresAt.After(now) {
		return errors.New("MFA challenge and session expiry must be in the future")
	}
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var locked uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT m.id
			FROM forge_memberships m
			JOIN forge_users u ON u.id = m.user_id
			JOIN forge_tenants t ON t.id = m.tenant_id
			JOIN forge_organizations o ON o.id = t.organization_id
			JOIN forge_totp_factors f ON f.user_id = u.id
			WHERE m.id = $1
			  AND m.status = 'active'
			  AND u.status = 'active'
			  AND t.status = 'active'
			  AND o.status = 'active'
			FOR UPDATE OF m, u, f
		`, membershipID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMFAIdentityRequired
		}
		if err != nil {
			return postgres.Translate(err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE forge_mfa_challenges
			SET consumed_at = COALESCE(consumed_at, now())
			WHERE membership_id = $1
			  AND consumed_at IS NULL
		`, membershipID); err != nil {
			return postgres.Translate(err)
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO forge_mfa_challenges
				(token_digest, membership_id, credential_version, factor_secret_digest,
				 expires_at, session_expires_at)
			SELECT $1, m.id, u.session_version, f.secret_digest, $4, $5
			FROM forge_memberships m
			JOIN forge_users u ON u.id = m.user_id
			JOIN forge_totp_factors f ON f.user_id = u.id
			WHERE m.id = $2
			  AND u.session_version = $3
			  AND m.status = 'active'
			  AND u.status = 'active'
		`, digest[:], membershipID, credentialVersion, expiresAt.UTC(), sessionExpiresAt.UTC())
		if err != nil {
			return postgres.Translate(err)
		}
		if tag.RowsAffected() != 1 {
			return auth.ErrCredentialsInvalid
		}
		return nil
	})
}

func (r *Repository) LoadMFAChallenge(ctx context.Context, digest auth.Digest) (auth.MFAChallengeRecord, bool, error) {
	var (
		record    auth.MFAChallengeRecord
		rawDigest []byte
	)
	err := r.db.QueryRow(ctx, `
		SELECT
			u.id,
			m.id,
			c.credential_version,
			f.secret_digest,
			f.secret_ciphertext,
			f.last_counter,
			c.expires_at,
			c.session_expires_at
		FROM forge_mfa_challenges c
		JOIN forge_memberships m ON m.id = c.membership_id
		JOIN forge_users u ON u.id = m.user_id
		JOIN forge_tenants t ON t.id = m.tenant_id
		JOIN forge_organizations o ON o.id = t.organization_id
		JOIN forge_totp_factors f
		  ON f.user_id = u.id
		 AND f.secret_digest = c.factor_secret_digest
		WHERE c.token_digest = $1
		  AND c.consumed_at IS NULL
		  AND c.expires_at > now()
		  AND c.session_expires_at > now()
		  AND c.credential_version = u.session_version
		  AND m.status = 'active'
		  AND u.status = 'active'
		  AND t.status = 'active'
		  AND o.status = 'active'
	`, digest[:]).Scan(
		&record.SubjectID,
		&record.MembershipID,
		&record.CredentialVersion,
		&rawDigest,
		&record.SecretCiphertext,
		&record.LastCounter,
		&record.ExpiresAt,
		&record.SessionExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.MFAChallengeRecord{}, false, nil
	}
	if err != nil {
		return auth.MFAChallengeRecord{}, false, postgres.Translate(err)
	}
	secretDigest, err := authDigest(rawDigest)
	if err != nil {
		return auth.MFAChallengeRecord{}, false, err
	}
	record.SecretDigest = secretDigest
	record.ExpiresAt = record.ExpiresAt.UTC()
	record.SessionExpiresAt = record.SessionExpiresAt.UTC()
	return record, true, nil
}

func (r *Repository) ConsumeMFAChallenge(
	ctx context.Context,
	digest auth.Digest,
	secretDigest auth.Digest,
	counter int64,
) (uuid.UUID, bool, error) {
	if counter < 0 {
		return uuid.UUID{}, false, errors.New("TOTP counter must be non-negative")
	}
	var membershipID uuid.UUID
	consumed := false
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var (
			subjectID   uuid.UUID
			lastCounter int64
		)
		err := tx.QueryRow(ctx, `
			SELECT u.id, m.id, f.last_counter
			FROM forge_mfa_challenges c
			JOIN forge_memberships m ON m.id = c.membership_id
			JOIN forge_users u ON u.id = m.user_id
			JOIN forge_tenants t ON t.id = m.tenant_id
			JOIN forge_organizations o ON o.id = t.organization_id
			JOIN forge_totp_factors f
			  ON f.user_id = u.id
			 AND f.secret_digest = c.factor_secret_digest
			WHERE c.token_digest = $1
			  AND c.factor_secret_digest = $2
			  AND c.consumed_at IS NULL
			  AND c.expires_at > now()
			  AND c.session_expires_at > now()
			  AND c.credential_version = u.session_version
			  AND m.status = 'active'
			  AND u.status = 'active'
			  AND t.status = 'active'
			  AND o.status = 'active'
			FOR UPDATE OF c, m, u, f
		`, digest[:], secretDigest[:]).Scan(&subjectID, &membershipID, &lastCounter)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return postgres.Translate(err)
		}
		if counter <= lastCounter {
			return nil
		}
		tag, err := tx.Exec(ctx, `
			UPDATE forge_totp_factors
			SET last_counter = $3, updated_at = now()
			WHERE user_id = $1
			  AND secret_digest = $2
			  AND last_counter < $3
		`, subjectID, secretDigest[:], counter)
		if err != nil {
			return postgres.Translate(err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("TOTP replay watermark changed while factor was locked")
		}
		tag, err = tx.Exec(ctx, `
			UPDATE forge_mfa_challenges
			SET consumed_at = now()
			WHERE token_digest = $1
			  AND consumed_at IS NULL
		`, digest[:])
		if err != nil {
			return postgres.Translate(err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("MFA challenge changed while it was locked")
		}
		consumed = true
		return nil
	})
	if err != nil || !consumed {
		return uuid.UUID{}, consumed, err
	}
	return membershipID, true, nil
}

func (r *Repository) ConsumeMFABackupChallenge(
	ctx context.Context,
	digest auth.Digest,
	backupCodeDigest auth.Digest,
) (uuid.UUID, bool, error) {
	var membershipID uuid.UUID
	consumed := false
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var backupID uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT m.id, b.id
			FROM forge_mfa_challenges c
			JOIN forge_memberships m ON m.id = c.membership_id
			JOIN forge_users u ON u.id = m.user_id
			JOIN forge_tenants t ON t.id = m.tenant_id
			JOIN forge_organizations o ON o.id = t.organization_id
			JOIN forge_totp_factors f
			  ON f.user_id = u.id
			 AND f.secret_digest = c.factor_secret_digest
			JOIN forge_mfa_backup_codes b
			  ON b.user_id = u.id
			 AND b.code_digest = $2
			 AND b.consumed_at IS NULL
			WHERE c.token_digest = $1
			  AND c.consumed_at IS NULL
			  AND c.expires_at > now()
			  AND c.session_expires_at > now()
			  AND c.credential_version = u.session_version
			  AND m.status = 'active'
			  AND u.status = 'active'
			  AND t.status = 'active'
			  AND o.status = 'active'
			FOR UPDATE OF c, m, u, f, b
		`, digest[:], backupCodeDigest[:]).Scan(&membershipID, &backupID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return postgres.Translate(err)
		}

		tag, err := tx.Exec(ctx, `
			UPDATE forge_mfa_backup_codes
			SET consumed_at = now()
			WHERE id = $1
			  AND consumed_at IS NULL
		`, backupID)
		if err != nil {
			return postgres.Translate(err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("MFA backup code changed while it was locked")
		}
		tag, err = tx.Exec(ctx, `
			UPDATE forge_mfa_challenges
			SET consumed_at = now()
			WHERE token_digest = $1
			  AND consumed_at IS NULL
		`, digest[:])
		if err != nil {
			return postgres.Translate(err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("MFA challenge changed while it was locked")
		}
		consumed = true
		return nil
	})
	if err != nil || !consumed {
		return uuid.UUID{}, consumed, err
	}
	return membershipID, true, nil
}

func (r *Repository) BeginTOTPRotation(
	ctx context.Context,
	subjectID uuid.UUID,
	secretDigest auth.Digest,
	secretCiphertext []byte,
	expiresAt time.Time,
) error {
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return errors.New("MFA subject ID must be a UUIDv7")
	}
	if len(secretCiphertext) == 0 {
		return errors.New("encrypted MFA secret is required")
	}
	if !expiresAt.After(r.now()) {
		return errors.New("MFA rotation expiry must be in the future")
	}
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var locked uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT u.id
			FROM forge_users u
			JOIN forge_totp_factors f ON f.user_id = u.id
			WHERE u.id = $1
			  AND u.status = 'active'
			FOR UPDATE OF u, f
		`, subjectID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.ErrMFANotEnabled
		}
		if err != nil {
			return postgres.Translate(err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO forge_totp_enrollments
				(user_id, secret_digest, secret_ciphertext, expires_at, created_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (user_id) DO UPDATE
			SET secret_digest = EXCLUDED.secret_digest,
			    secret_ciphertext = EXCLUDED.secret_ciphertext,
			    expires_at = EXCLUDED.expires_at,
			    created_at = now()
		`, subjectID, secretDigest[:], secretCiphertext, expiresAt.UTC())
		return postgres.Translate(err)
	})
}

func (r *Repository) ConfirmTOTPRotation(
	ctx context.Context,
	subjectID uuid.UUID,
	secretDigest auth.Digest,
	counter int64,
	backupCodeDigests []auth.Digest,
) (bool, error) {
	if counter < 0 {
		return false, errors.New("TOTP counter must be non-negative")
	}
	if len(backupCodeDigests) != auth.DefaultMFABackupCodeCount {
		return false, errors.New("TOTP rotation requires the default MFA backup code set")
	}
	confirmed := false
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var (
			ciphertext []byte
			rawDigest  []byte
		)
		err := tx.QueryRow(ctx, `
			SELECT e.secret_ciphertext, e.secret_digest
			FROM forge_totp_enrollments e
			JOIN forge_users u ON u.id = e.user_id
			JOIN forge_totp_factors f ON f.user_id = u.id
			WHERE e.user_id = $1
			  AND e.secret_digest = $2
			  AND e.expires_at > now()
			  AND u.status = 'active'
			FOR UPDATE OF e, u, f
		`, subjectID, secretDigest[:]).Scan(&ciphertext, &rawDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return postgres.Translate(err)
		}
		storedDigest, err := authDigest(rawDigest)
		if err != nil {
			return err
		}
		if storedDigest != secretDigest {
			return errors.New("MFA rotation digest changed while locked")
		}

		tag, err := tx.Exec(ctx, `
			UPDATE forge_totp_factors
			SET secret_digest = $2,
			    secret_ciphertext = $3,
			    last_counter = $4,
			    updated_at = now()
			WHERE user_id = $1
		`, subjectID, secretDigest[:], ciphertext, counter)
		if err != nil {
			return postgres.Translate(err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("active TOTP factor disappeared while rotating")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM forge_mfa_backup_codes WHERE user_id = $1`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		for _, backupDigest := range backupCodeDigests {
			if _, err := tx.Exec(ctx, `
				INSERT INTO forge_mfa_backup_codes (user_id, code_digest)
				VALUES ($1, $2)
			`, subjectID, backupDigest[:]); err != nil {
				return postgres.Translate(err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE forge_users
			SET session_version = session_version + 1, updated_at = now()
			WHERE id = $1 AND status = 'active'
		`, subjectID); err != nil {
			return postgres.Translate(err)
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
			UPDATE forge_mfa_challenges c
			SET consumed_at = COALESCE(c.consumed_at, now())
			FROM forge_memberships m
			WHERE c.membership_id = m.id
			  AND m.user_id = $1
			  AND c.consumed_at IS NULL
		`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM forge_totp_enrollments WHERE user_id = $1`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		confirmed = true
		return nil
	})
	return confirmed, err
}

func (r *Repository) DisableMFA(ctx context.Context, subjectID uuid.UUID) (bool, error) {
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return false, errors.New("MFA subject ID must be a UUIDv7")
	}
	disabled := false
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var locked uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT u.id
			FROM forge_users u
			JOIN forge_totp_factors f ON f.user_id = u.id
			WHERE u.id = $1
			  AND u.status = 'active'
			FOR UPDATE OF u, f
		`, subjectID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return postgres.Translate(err)
		}

		if _, err := tx.Exec(ctx, `DELETE FROM forge_mfa_backup_codes WHERE user_id = $1`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM forge_totp_enrollments WHERE user_id = $1`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM forge_totp_factors WHERE user_id = $1`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE forge_users
			SET session_version = session_version + 1, updated_at = now()
			WHERE id = $1 AND status = 'active'
		`, subjectID); err != nil {
			return postgres.Translate(err)
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
			UPDATE forge_mfa_challenges c
			SET consumed_at = COALESCE(c.consumed_at, now())
			FROM forge_memberships m
			WHERE c.membership_id = m.id
			  AND m.user_id = $1
			  AND c.consumed_at IS NULL
		`, subjectID); err != nil {
			return postgres.Translate(err)
		}
		disabled = true
		return nil
	})
	return disabled, err
}

// PruneMFA deletes expired enrollment rows and consumed/expired login
// challenges in bounded batches. Each category is capped at limit.
func (r *Repository) PruneMFA(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, errors.New("prune limit must be positive")
	}
	var total int64
	for _, query := range []string{
		`WITH doomed AS (
			SELECT user_id
			FROM forge_totp_enrollments
			WHERE expires_at <= clock_timestamp()
			ORDER BY expires_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM forge_totp_enrollments target
		USING doomed
		WHERE target.user_id = doomed.user_id`,
		`WITH doomed AS (
			SELECT id
			FROM forge_mfa_challenges
			WHERE consumed_at IS NOT NULL
			   OR expires_at <= clock_timestamp()
			   OR session_expires_at <= clock_timestamp()
			ORDER BY COALESCE(consumed_at, expires_at)
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM forge_mfa_challenges target
		USING doomed
		WHERE target.id = doomed.id`,
	} {
		tag, err := r.db.Exec(ctx, query, limit)
		if err != nil {
			return total, postgres.Translate(err)
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

func authDigest(raw []byte) (auth.Digest, error) {
	if len(raw) != len(auth.Digest{}) {
		return auth.Digest{}, errors.New("identity store contains an invalid digest")
	}
	var digest auth.Digest
	copy(digest[:], raw)
	return digest, nil
}
