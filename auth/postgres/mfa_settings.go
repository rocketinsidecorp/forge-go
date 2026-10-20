package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var (
	_ auth.MFASettingsStore     = (*Repository)(nil)
	_ auth.SubjectPasswordStore = (*Repository)(nil)
)

// LookupSubjectPassword returns the password credential of an active user,
// for step-up authentication of a signed-in account. Login is the canonical
// login (email_normalized).
func (r *Repository) LookupSubjectPassword(ctx context.Context, subjectID uuid.UUID) (auth.SubjectPassword, bool, error) {
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return auth.SubjectPassword{}, false, errors.New("user ID must be a UUIDv7")
	}
	var credential auth.SubjectPassword
	err := r.db.QueryRow(ctx, `
		SELECT id, email_normalized, password_hash
		FROM forge_users
		WHERE id = $1 AND status = 'active'
	`, subjectID).Scan(&credential.SubjectID, &credential.Login, &credential.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.SubjectPassword{}, false, nil
	}
	if err != nil {
		return auth.SubjectPassword{}, false, postgres.Translate(err)
	}
	return credential, true, nil
}

// LoadMFAStatus reports whether an active user has a TOTP factor and how
// many of its backup codes are unused.
func (r *Repository) LoadMFAStatus(ctx context.Context, subjectID uuid.UUID) (auth.MFAStatus, error) {
	var (
		status    auth.MFAStatus
		remaining int64
	)
	err := r.db.QueryRow(ctx, `
		SELECT
			EXISTS (
				SELECT 1
				FROM forge_totp_factors f
				JOIN forge_users u ON u.id = f.user_id
				WHERE f.user_id = $1 AND u.status = 'active'
			),
			(SELECT count(*) FROM forge_mfa_backup_codes WHERE user_id = $1 AND consumed_at IS NULL)
	`, subjectID).Scan(&status.Enabled, &remaining)
	if err != nil {
		return auth.MFAStatus{}, postgres.Translate(err)
	}
	if status.Enabled {
		status.BackupCodesRemaining = int(remaining)
	}
	return status, nil
}

// LoadActiveTOTP returns the active factor of an active user, with its
// replay watermark.
func (r *Repository) LoadActiveTOTP(ctx context.Context, subjectID uuid.UUID) (auth.TOTPSecretRecord, bool, error) {
	var (
		record    auth.TOTPSecretRecord
		rawDigest []byte
	)
	err := r.db.QueryRow(ctx, `
		SELECT f.user_id, f.secret_digest, f.secret_ciphertext, f.last_counter
		FROM forge_totp_factors f
		JOIN forge_users u ON u.id = f.user_id
		WHERE f.user_id = $1 AND u.status = 'active'
	`, subjectID).Scan(&record.SubjectID, &rawDigest, &record.SecretCiphertext, &record.LastCounter)
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
	return record, true, nil
}

// AdvanceTOTPCounter records counter as the factor's last accepted period
// when it is later than the stored one, in one statement: of two concurrent
// requests with the same code only one advances it.
func (r *Repository) AdvanceTOTPCounter(ctx context.Context, subjectID uuid.UUID, secretDigest auth.Digest, counter int64) (bool, error) {
	if counter < 0 {
		return false, errors.New("TOTP counter must be non-negative")
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE forge_totp_factors f
		SET last_counter = $3, updated_at = now()
		FROM forge_users u
		WHERE f.user_id = $1
		  AND u.id = f.user_id
		  AND u.status = 'active'
		  AND f.secret_digest = $2
		  AND f.last_counter < $3
	`, subjectID, secretDigest[:], counter)
	if err != nil {
		return false, postgres.Translate(err)
	}
	return tag.RowsAffected() == 1, nil
}

// ConsumeMFABackupCode marks one unused backup code of an active user with a
// factor used, in one statement.
func (r *Repository) ConsumeMFABackupCode(ctx context.Context, subjectID uuid.UUID, codeDigest auth.Digest) (bool, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE forge_mfa_backup_codes b
		SET consumed_at = now()
		WHERE b.user_id = $1
		  AND b.code_digest = $2
		  AND b.consumed_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM forge_totp_factors f
			JOIN forge_users u ON u.id = f.user_id
			WHERE f.user_id = $1 AND u.status = 'active'
		  )
	`, subjectID, codeDigest[:])
	if err != nil {
		return false, postgres.Translate(err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReplaceMFABackupCodes replaces every backup code of an active user with a
// factor, in one transaction holding the user and factor rows.
func (r *Repository) ReplaceMFABackupCodes(ctx context.Context, subjectID uuid.UUID, codeDigests []auth.Digest) (bool, error) {
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return false, errors.New("MFA subject ID must be a UUIDv7")
	}
	if len(codeDigests) != auth.DefaultMFABackupCodeCount {
		return false, errors.New("MFA backup code regeneration requires the default backup code set")
	}
	replaced := false
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
		for _, digest := range codeDigests {
			if _, err := tx.Exec(ctx, `
				INSERT INTO forge_mfa_backup_codes (user_id, code_digest)
				VALUES ($1, $2)
			`, subjectID, digest[:]); err != nil {
				return postgres.Translate(err)
			}
		}
		replaced = true
		return nil
	})
	return replaced, err
}
