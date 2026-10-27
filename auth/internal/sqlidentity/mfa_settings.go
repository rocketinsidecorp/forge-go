package sqlidentity

import (
	"context"
	"database/sql"
	"errors"

	"github.com/rocketinsidecorp/forge-go/auth"
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
	if !isV7(subjectID) {
		return auth.SubjectPassword{}, false, errors.New("user ID must be a UUIDv7")
	}
	var credential auth.SubjectPassword
	err := r.db.QueryRowContext(ctx, r.q(`
		SELECT id, email_normalized, password_hash
		FROM forge_users
		WHERE id = ? AND status = 'active'
	`), subjectID).Scan(&credential.SubjectID, &credential.Login, &credential.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.SubjectPassword{}, false, nil
	}
	if err != nil {
		return auth.SubjectPassword{}, false, r.translate(err)
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
	err := r.db.QueryRowContext(ctx, r.q(`
		SELECT
			EXISTS (
				SELECT 1
				FROM forge_totp_factors f
				JOIN forge_users u ON u.id = f.user_id
				WHERE f.user_id = ? AND u.status = 'active'
			),
			(SELECT count(*) FROM forge_mfa_backup_codes WHERE user_id = ? AND consumed_at IS NULL)
	`), subjectID, subjectID).Scan(&status.Enabled, &remaining)
	if err != nil {
		return auth.MFAStatus{}, r.translate(err)
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
	err := r.db.QueryRowContext(ctx, r.q(`
		SELECT f.user_id, f.secret_digest, f.secret_ciphertext, f.last_counter
		FROM forge_totp_factors f
		JOIN forge_users u ON u.id = f.user_id
		WHERE f.user_id = ? AND u.status = 'active'
	`), subjectID).Scan(&record.SubjectID, &rawDigest, &record.SecretCiphertext, &record.LastCounter)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.TOTPSecretRecord{}, false, nil
	}
	if err != nil {
		return auth.TOTPSecretRecord{}, false, r.translate(err)
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
	result, err := r.db.ExecContext(ctx, r.q(`
		UPDATE forge_totp_factors
		SET last_counter = ?, updated_at = {now}
		WHERE user_id = ?
		  AND secret_digest = ?
		  AND last_counter < ?
		  AND EXISTS (SELECT 1 FROM forge_users u WHERE u.id = forge_totp_factors.user_id AND u.status = 'active')
	`), counter, subjectID, secretDigest[:], counter)
	return affectedOne(result, err, r.translate)
}

// ConsumeMFABackupCode marks one unused backup code of an active user with a
// factor used, in one statement.
func (r *Repository) ConsumeMFABackupCode(ctx context.Context, subjectID uuid.UUID, codeDigest auth.Digest) (bool, error) {
	result, err := r.db.ExecContext(ctx, r.q(`
		UPDATE forge_mfa_backup_codes
		SET consumed_at = {now}
		WHERE user_id = ?
		  AND code_digest = ?
		  AND consumed_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM forge_totp_factors f
			JOIN forge_users u ON u.id = f.user_id
			WHERE f.user_id = ? AND u.status = 'active'
		  )
	`), subjectID, codeDigest[:], subjectID)
	return affectedOne(result, err, r.translate)
}

// HasMFABackupCode reports whether codeDigest is an unused backup code of an
// active user with a factor, without using it.
func (r *Repository) HasMFABackupCode(ctx context.Context, subjectID uuid.UUID, codeDigest auth.Digest) (bool, error) {
	var unused bool
	err := r.db.QueryRowContext(ctx, r.q(`
		SELECT EXISTS (
			SELECT 1
			FROM forge_mfa_backup_codes b
			JOIN forge_totp_factors f ON f.user_id = b.user_id
			JOIN forge_users u ON u.id = b.user_id
			WHERE b.user_id = ?
			  AND b.code_digest = ?
			  AND b.consumed_at IS NULL
			  AND u.status = 'active'
		)
	`), subjectID, codeDigest[:]).Scan(&unused)
	if err != nil {
		return false, r.translate(err)
	}
	return unused, nil
}

// ReplaceMFABackupCodes replaces every backup code of an active user with a
// factor, in one transaction holding the user and factor rows, using up
// stepUp first (auth.ErrMFAStepUpInvalid, nothing replaced, when it is not an
// unused code).
func (r *Repository) ReplaceMFABackupCodes(ctx context.Context, subjectID uuid.UUID, codeDigests []auth.Digest, stepUp *auth.Digest) (bool, error) {
	if !isV7(subjectID) {
		return false, errors.New("MFA subject ID must be a UUIDv7")
	}
	if len(codeDigests) != auth.DefaultMFABackupCodeCount {
		return false, errors.New("MFA backup code regeneration requires the default backup code set")
	}
	replaced := false
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		locked, err := r.lockActiveFactorUser(ctx, tx, subjectID)
		if err != nil || !locked {
			return err
		}
		if err := r.spendStepUp(ctx, tx, subjectID, stepUp); err != nil {
			return err
		}
		if err := r.replaceBackupCodes(ctx, tx, subjectID, codeDigests); err != nil {
			return err
		}
		replaced = true
		return nil
	})
	return replaced, err
}
