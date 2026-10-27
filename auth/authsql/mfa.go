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
	// ErrMFAIdentityRequired is returned when the account named for an MFA
	// operation is missing or not active.
	ErrMFAIdentityRequired = errors.New("active MFA identity is required")

	_ auth.MFAStore          = (*Repository)(nil)
	_ auth.MFALifecycleStore = (*Repository)(nil)
)

// Lock order, as in auth/postgres: every MFA transaction locks the account's
// forge_users row first (lockUser), and only then its enrollment, factor,
// backup code, membership, session and challenge rows, so MFA transactions of
// one account run one at a time and no two of them can deadlock. On SQLite
// the transaction holds the database's write lock from its first statement
// (BEGIN IMMEDIATE), which serializes them as well.

// challengeUserQuery names the user of the challenge whose digest is the
// parameter, without locking anything.
const challengeUserQuery = `
	SELECT m.user_id
	FROM forge_mfa_challenges c
	JOIN forge_memberships m ON m.id = c.membership_id
	WHERE c.token_digest = ?
`

// lockUserOf locks the user row of the account query names; false when it
// names none.
func (r *Repository) lockUserOf(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	subjectID, found, err := r.userOf(ctx, tx, query, args...)
	if err != nil || !found {
		return false, err
	}
	return r.lockUser(ctx, tx, subjectID, false)
}

// spendStepUp uses up stepUp, a backup code given for step-up authentication
// of the change the transaction applies, with the user row locked:
// auth.ErrMFAStepUpInvalid (the transaction rolls back) when it is not an
// unused code of the account.
func (r *Repository) spendStepUp(ctx context.Context, tx *sql.Tx, subjectID uuid.UUID, stepUp *auth.Digest) error {
	if stepUp == nil {
		return nil
	}
	result, err := tx.ExecContext(ctx, r.q(`
		UPDATE forge_mfa_backup_codes
		SET consumed_at = {now}
		WHERE user_id = ?
		  AND code_digest = ?
		  AND consumed_at IS NULL
	`), subjectID, stepUp[:])
	spent, err := affectedOne(result, err, r.translate)
	if err != nil {
		return err
	}
	if !spent {
		return auth.ErrMFAStepUpInvalid
	}
	return nil
}

// lockActiveFactorUser locks an active user that has a TOTP factor, and the
// factor; false when there is none.
func (r *Repository) lockActiveFactorUser(ctx context.Context, tx *sql.Tx, subjectID uuid.UUID) (bool, error) {
	var locked uuid.UUID
	err := tx.QueryRowContext(ctx, r.q(`
		SELECT u.id
		FROM forge_users u
		JOIN forge_totp_factors f ON f.user_id = u.id
		WHERE u.id = ?
		  AND u.status = 'active'{lock}
	`), subjectID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, r.translate(err)
	}
	return true, nil
}

// hasFactor reports whether subjectID has a TOTP factor.
func (r *Repository) hasFactor(ctx context.Context, tx *sql.Tx, subjectID uuid.UUID) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, r.q(`SELECT EXISTS (SELECT 1 FROM forge_totp_factors WHERE user_id = ?)`), subjectID).Scan(&exists); err != nil {
		return false, r.translate(err)
	}
	return exists, nil
}

// storePendingTOTP replaces the pending enrollment (or rotation) secret of
// subjectID, with the user row locked.
func (r *Repository) storePendingTOTP(ctx context.Context, tx *sql.Tx, subjectID uuid.UUID, secretDigest auth.Digest, secretCiphertext []byte, expiresAt time.Time) error {
	if _, err := tx.ExecContext(ctx, r.q(`DELETE FROM forge_totp_enrollments WHERE user_id = ?`), subjectID); err != nil {
		return r.translate(err)
	}
	_, err := tx.ExecContext(ctx, r.q(`
		INSERT INTO forge_totp_enrollments
			(user_id, secret_digest, secret_ciphertext, expires_at, created_at)
		VALUES (?, ?, ?, ?, {now})
	`), subjectID, secretDigest[:], secretCiphertext, r.dialect.Time(expiresAt))
	return r.translate(err)
}

// BeginTOTPEnrollment stores a pending TOTP secret (its digest and
// ciphertext) for an active account that has no factor yet, replacing an
// earlier pending one. It returns auth.ErrMFAAlreadyEnabled when a factor is
// already active.
func (r *Repository) BeginTOTPEnrollment(ctx context.Context, subjectID uuid.UUID, secretDigest auth.Digest, secretCiphertext []byte, expiresAt time.Time) error {
	if !isV7(subjectID) {
		return errors.New("MFA subject ID must be a UUIDv7")
	}
	if len(secretCiphertext) == 0 {
		return errors.New("encrypted MFA secret is required")
	}
	if !expiresAt.After(r.now()) {
		return errors.New("MFA enrollment expiry must be in the future")
	}
	return r.inTx(ctx, func(tx *sql.Tx) error {
		locked, err := r.lockUser(ctx, tx, subjectID, true)
		if err != nil {
			return err
		}
		if !locked {
			return ErrMFAIdentityRequired
		}
		enabled, err := r.hasFactor(ctx, tx, subjectID)
		if err != nil {
			return err
		}
		if enabled {
			return auth.ErrMFAAlreadyEnabled
		}
		return r.storePendingTOTP(ctx, tx, subjectID, secretDigest, secretCiphertext, expiresAt)
	})
}

// LoadPendingTOTP returns the account's unexpired pending TOTP secret. found
// is false when there is none.
func (r *Repository) LoadPendingTOTP(ctx context.Context, subjectID uuid.UUID) (auth.TOTPSecretRecord, bool, error) {
	var (
		record    auth.TOTPSecretRecord
		rawDigest []byte
		expiresAt timestamp
	)
	// One statement, one snapshot: the key is a rotation's exactly when the
	// account has a factor.
	err := r.db.QueryRowContext(ctx, r.q(`
		SELECT e.user_id, e.secret_digest, e.secret_ciphertext, e.expires_at,
		       EXISTS (SELECT 1 FROM forge_totp_factors f WHERE f.user_id = e.user_id)
		FROM forge_totp_enrollments e
		JOIN forge_users u ON u.id = e.user_id
		WHERE e.user_id = ?
		  AND u.status = 'active'
	`), subjectID).Scan(&record.SubjectID, &rawDigest, &record.SecretCiphertext, &expiresAt, &record.FactorActive)
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
	record.ExpiresAt = expiresAt.Time
	return record, true, nil
}

// lockPending locks the unexpired pending secret of subjectID whose digest
// is secretDigest (with the account's factor when rotation) and returns its
// ciphertext; nil when there is none.
func (r *Repository) lockPending(ctx context.Context, tx *sql.Tx, subjectID uuid.UUID, secretDigest auth.Digest, rotation bool) ([]byte, error) {
	query := `
		SELECT e.secret_ciphertext, e.secret_digest
		FROM forge_totp_enrollments e
		JOIN forge_users u ON u.id = e.user_id`
	if rotation {
		query += `
		JOIN forge_totp_factors f ON f.user_id = u.id`
	}
	query += `
		WHERE e.user_id = ?
		  AND e.secret_digest = ?
		  AND e.expires_at > {now}
		  AND u.status = 'active'{lock}`
	var ciphertext, rawDigest []byte
	err := tx.QueryRowContext(ctx, r.q(query), subjectID, secretDigest[:]).Scan(&ciphertext, &rawDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, r.translate(err)
	}
	stored, err := authDigest(rawDigest)
	if err != nil {
		return nil, err
	}
	if stored != secretDigest {
		return nil, errors.New("MFA enrollment digest changed while locked")
	}
	return ciphertext, nil
}

// ConfirmTOTPEnrollment activates the pending factor whose digest is
// secretDigest, records counter as the last accepted time step and stores the
// digests of the backup codes, all in one transaction. It reports false when
// nothing matched.
func (r *Repository) ConfirmTOTPEnrollment(ctx context.Context, subjectID uuid.UUID, secretDigest auth.Digest, counter int64, backupCodeDigests []auth.Digest) (bool, error) {
	if counter < 0 {
		return false, errors.New("TOTP counter must be non-negative")
	}
	if len(backupCodeDigests) != auth.DefaultMFABackupCodeCount {
		return false, errors.New("TOTP confirmation requires the default MFA backup code set")
	}
	confirmed := false
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		if locked, err := r.lockUser(ctx, tx, subjectID, false); err != nil || !locked {
			return err
		}
		ciphertext, err := r.lockPending(ctx, tx, subjectID, secretDigest, false)
		if err != nil || ciphertext == nil {
			return err
		}
		// The pending key of a rotation sits in the same table as an
		// enrollment's: an account that has a factor is refused here,
		// explicitly, rather than by the INSERT failing on its key.
		enabled, err := r.hasFactor(ctx, tx, subjectID)
		if err != nil {
			return err
		}
		if enabled {
			return auth.ErrMFAAlreadyEnabled
		}
		result, err := tx.ExecContext(ctx, r.q(`
			INSERT INTO forge_totp_factors
				(user_id, secret_digest, secret_ciphertext, last_counter, enabled_at, updated_at)
			VALUES (?, ?, ?, ?, {now}, {now})
		`), subjectID, secretDigest[:], ciphertext, counter)
		created, err := affectedOne(result, err, r.translate)
		if err != nil {
			return err
		}
		if !created {
			return errors.New("TOTP factor was not created while enrollment was locked")
		}
		if err := r.replaceBackupCodes(ctx, tx, subjectID, backupCodeDigests); err != nil {
			return err
		}
		if err := r.endCredentialVersion(ctx, tx, subjectID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, r.q(`DELETE FROM forge_totp_enrollments WHERE user_id = ?`), subjectID); err != nil {
			return r.translate(err)
		}
		confirmed = true
		return nil
	})
	return confirmed, err
}

// endCredentialVersion increments the account's session version and revokes
// its sessions and pending MFA challenges: a factor change signs the account
// out everywhere.
func (r *Repository) endCredentialVersion(ctx context.Context, tx *sql.Tx, subjectID uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, r.q(`
		UPDATE forge_users
		SET session_version = session_version + 1, updated_at = {now}
		WHERE id = ? AND status = 'active'
	`), subjectID); err != nil {
		return r.translate(err)
	}
	if err := r.revokeSessionsOf(ctx, tx, subjectID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, r.q(`
		UPDATE forge_mfa_challenges
		SET consumed_at = {now}
		WHERE consumed_at IS NULL
		  AND membership_id IN (SELECT id FROM forge_memberships WHERE user_id = ?)
	`), subjectID)
	return r.translate(err)
}

// replaceBackupCodes deletes every backup code of subjectID and stores
// digests instead.
func (r *Repository) replaceBackupCodes(ctx context.Context, tx *sql.Tx, subjectID uuid.UUID, digests []auth.Digest) error {
	if _, err := tx.ExecContext(ctx, r.q(`DELETE FROM forge_mfa_backup_codes WHERE user_id = ?`), subjectID); err != nil {
		return r.translate(err)
	}
	for _, digest := range digests {
		// IDs come from the application (UUIDv7), not a database default.
		id, err := uuid.New()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, r.q(`
			INSERT INTO forge_mfa_backup_codes (id, user_id, code_digest, created_at)
			VALUES (?, ?, ?, {now})
		`), id, subjectID, digest[:]); err != nil {
			return r.translate(err)
		}
	}
	return nil
}

// CreateMFAChallenge stores the digest of a login challenge for
// membershipID, valid until expiresAt, bound to credentialVersion and the
// account's current factor. The session created on completion expires at
// sessionExpiresAt.
func (r *Repository) CreateMFAChallenge(ctx context.Context, digest auth.Digest, membershipID uuid.UUID, credentialVersion int64, expiresAt, sessionExpiresAt time.Time) error {
	if !isV7(membershipID) {
		return errors.New("MFA membership ID must be a UUIDv7")
	}
	if credentialVersion < 1 {
		return errors.New("MFA credential version must be positive")
	}
	now := r.now()
	if !expiresAt.After(now) || !sessionExpiresAt.After(now) {
		return errors.New("MFA challenge and session expiry must be in the future")
	}
	return r.inTx(ctx, func(tx *sql.Tx) error {
		userLocked, err := r.lockUserOf(ctx, tx, `SELECT user_id FROM forge_memberships WHERE id = ?`, membershipID)
		if err != nil {
			return err
		}
		if !userLocked {
			return ErrMFAIdentityRequired
		}
		var locked uuid.UUID
		err = tx.QueryRowContext(ctx, r.q(`
			SELECT m.id
			FROM forge_memberships m
			JOIN forge_users u ON u.id = m.user_id
			JOIN forge_tenants t ON t.id = m.tenant_id
			JOIN forge_organizations o ON o.id = t.organization_id
			JOIN forge_totp_factors f ON f.user_id = u.id
			WHERE m.id = ?`+activeChain+`{lock}
		`), membershipID).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMFAIdentityRequired
		}
		if err != nil {
			return r.translate(err)
		}
		if _, err := tx.ExecContext(ctx, r.q(`
			UPDATE forge_mfa_challenges
			SET consumed_at = {now}
			WHERE membership_id = ?
			  AND consumed_at IS NULL
		`), membershipID); err != nil {
			return r.translate(err)
		}
		id, err := uuid.New()
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, r.q(`
			INSERT INTO forge_mfa_challenges
				(id, token_digest, membership_id, credential_version, factor_secret_digest,
				 expires_at, session_expires_at, created_at)
			SELECT ?, ?, m.id, u.session_version, f.secret_digest, ?, ?, {now}
			FROM forge_memberships m
			JOIN forge_users u ON u.id = m.user_id
			JOIN forge_totp_factors f ON f.user_id = u.id
			WHERE m.id = ?
			  AND u.session_version = ?
			  AND m.status = 'active'
			  AND u.status = 'active'
		`), id, digest[:], r.dialect.Time(expiresAt), r.dialect.Time(sessionExpiresAt), membershipID, credentialVersion)
		created, err := affectedOne(result, err, r.translate)
		if err != nil {
			return err
		}
		if !created {
			return auth.ErrCredentialsInvalid
		}
		return nil
	})
}

// liveChallenge is the join and condition of a challenge that can still be
// completed: unconsumed, unexpired, at the user's credential version, for an
// active chain and the factor it was issued for.
const liveChallenge = `
		FROM forge_mfa_challenges c
		JOIN forge_memberships m ON m.id = c.membership_id
		JOIN forge_users u ON u.id = m.user_id
		JOIN forge_tenants t ON t.id = m.tenant_id
		JOIN forge_organizations o ON o.id = t.organization_id
		JOIN forge_totp_factors f
		  ON f.user_id = u.id
		 AND f.secret_digest = c.factor_secret_digest`

const liveChallengeCondition = `
		  AND c.consumed_at IS NULL
		  AND c.expires_at > {now}
		  AND c.session_expires_at > {now}
		  AND c.credential_version = u.session_version` + activeChain

// LoadMFAChallenge returns the unconsumed, unexpired challenge with digest
// and the factor secret needed to verify its code. found is false when it no
// longer qualifies.
func (r *Repository) LoadMFAChallenge(ctx context.Context, digest auth.Digest) (auth.MFAChallengeRecord, bool, error) {
	var (
		record                      auth.MFAChallengeRecord
		rawDigest                   []byte
		expiresAt, sessionExpiresAt timestamp
	)
	err := r.db.QueryRowContext(ctx, r.q(`
		SELECT u.id, m.id, c.credential_version, f.secret_digest, f.secret_ciphertext,
		       f.last_counter, c.expires_at, c.session_expires_at`+liveChallenge+`
		WHERE c.token_digest = ?`+liveChallengeCondition), digest[:]).Scan(
		&record.SubjectID,
		&record.MembershipID,
		&record.CredentialVersion,
		&rawDigest,
		&record.SecretCiphertext,
		&record.LastCounter,
		&expiresAt,
		&sessionExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.MFAChallengeRecord{}, false, nil
	}
	if err != nil {
		return auth.MFAChallengeRecord{}, false, r.translate(err)
	}
	secretDigest, err := authDigest(rawDigest)
	if err != nil {
		return auth.MFAChallengeRecord{}, false, err
	}
	record.SecretDigest = secretDigest
	record.ExpiresAt = expiresAt.Time
	record.SessionExpiresAt = sessionExpiresAt.Time
	return record, true, nil
}

// consumeChallenge marks the challenge with digest consumed, with it locked.
func (r *Repository) consumeChallenge(ctx context.Context, tx *sql.Tx, digest auth.Digest) error {
	result, err := tx.ExecContext(ctx, r.q(`
		UPDATE forge_mfa_challenges
		SET consumed_at = {now}
		WHERE token_digest = ?
		  AND consumed_at IS NULL
	`), digest[:])
	consumed, err := affectedOne(result, err, r.translate)
	if err != nil {
		return err
	}
	if !consumed {
		return errors.New("MFA challenge changed while it was locked")
	}
	return nil
}

// ConsumeMFAChallenge consumes the challenge and advances the factor's last
// counter to counter in one transaction, so a challenge or a TOTP code is
// accepted at most once. It returns the membership to sign in and false when
// the challenge, the secret digest or the counter does not qualify.
func (r *Repository) ConsumeMFAChallenge(ctx context.Context, digest auth.Digest, secretDigest auth.Digest, counter int64) (uuid.UUID, bool, error) {
	if counter < 0 {
		return uuid.UUID{}, false, errors.New("TOTP counter must be non-negative")
	}
	var membershipID uuid.UUID
	consumed := false
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		if locked, err := r.lockUserOf(ctx, tx, challengeUserQuery, digest[:]); err != nil || !locked {
			return err
		}
		var (
			subjectID   uuid.UUID
			lastCounter int64
		)
		err := tx.QueryRowContext(ctx, r.q(`
			SELECT u.id, m.id, f.last_counter`+liveChallenge+`
			WHERE c.token_digest = ?
			  AND c.factor_secret_digest = ?`+liveChallengeCondition+`{lock}
		`), digest[:], secretDigest[:]).Scan(&subjectID, &membershipID, &lastCounter)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return r.translate(err)
		}
		if counter <= lastCounter {
			return nil
		}
		result, err := tx.ExecContext(ctx, r.q(`
			UPDATE forge_totp_factors
			SET last_counter = ?, updated_at = {now}
			WHERE user_id = ?
			  AND secret_digest = ?
			  AND last_counter < ?
		`), counter, subjectID, secretDigest[:], counter)
		advanced, err := affectedOne(result, err, r.translate)
		if err != nil {
			return err
		}
		if !advanced {
			return errors.New("TOTP replay watermark changed while factor was locked")
		}
		if err := r.consumeChallenge(ctx, tx, digest); err != nil {
			return err
		}
		consumed = true
		return nil
	})
	if err != nil || !consumed {
		return uuid.UUID{}, consumed, err
	}
	return membershipID, true, nil
}

// ConsumeMFABackupChallenge consumes the challenge and spends the unused
// backup code with backupCodeDigest in one transaction. It returns the
// membership to sign in and false when either does not qualify.
func (r *Repository) ConsumeMFABackupChallenge(ctx context.Context, digest auth.Digest, backupCodeDigest auth.Digest) (uuid.UUID, bool, error) {
	var membershipID uuid.UUID
	consumed := false
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		if locked, err := r.lockUserOf(ctx, tx, challengeUserQuery, digest[:]); err != nil || !locked {
			return err
		}
		var backupID uuid.UUID
		err := tx.QueryRowContext(ctx, r.q(`
			SELECT m.id, b.id`+liveChallenge+`
			JOIN forge_mfa_backup_codes b
			  ON b.user_id = u.id
			 AND b.code_digest = ?
			 AND b.consumed_at IS NULL
			WHERE c.token_digest = ?`+liveChallengeCondition+`{lock}
		`), backupCodeDigest[:], digest[:]).Scan(&membershipID, &backupID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return r.translate(err)
		}
		result, err := tx.ExecContext(ctx, r.q(`
			UPDATE forge_mfa_backup_codes
			SET consumed_at = {now}
			WHERE id = ?
			  AND consumed_at IS NULL
		`), backupID)
		spent, err := affectedOne(result, err, r.translate)
		if err != nil {
			return err
		}
		if !spent {
			return errors.New("MFA backup code changed while it was locked")
		}
		if err := r.consumeChallenge(ctx, tx, digest); err != nil {
			return err
		}
		consumed = true
		return nil
	})
	if err != nil || !consumed {
		return uuid.UUID{}, consumed, err
	}
	return membershipID, true, nil
}

// BeginTOTPRotation stores a pending replacement secret for an account with
// an active factor, which stays active until the replacement is confirmed. A
// non-nil stepUp backup code digest is spent in the same transaction;
// auth.ErrMFAStepUpInvalid means it was not an unused code of the account.
func (r *Repository) BeginTOTPRotation(ctx context.Context, subjectID uuid.UUID, secretDigest auth.Digest, secretCiphertext []byte, expiresAt time.Time, stepUp *auth.Digest) error {
	if !isV7(subjectID) {
		return errors.New("MFA subject ID must be a UUIDv7")
	}
	if len(secretCiphertext) == 0 {
		return errors.New("encrypted MFA secret is required")
	}
	if !expiresAt.After(r.now()) {
		return errors.New("MFA rotation expiry must be in the future")
	}
	return r.inTx(ctx, func(tx *sql.Tx) error {
		locked, err := r.lockActiveFactorUser(ctx, tx, subjectID)
		if err != nil {
			return err
		}
		if !locked {
			return auth.ErrMFANotEnabled
		}
		if err := r.spendStepUp(ctx, tx, subjectID, stepUp); err != nil {
			return err
		}
		return r.storePendingTOTP(ctx, tx, subjectID, secretDigest, secretCiphertext, expiresAt)
	})
}

// ConfirmTOTPRotation replaces the active factor with the pending one whose
// digest is secretDigest, records counter, replaces the backup codes with new
// ones and spends stepUp when given, all in one transaction. It reports false
// when nothing matched.
func (r *Repository) ConfirmTOTPRotation(ctx context.Context, subjectID uuid.UUID, secretDigest auth.Digest, counter int64, backupCodeDigests []auth.Digest, stepUp *auth.Digest) (bool, error) {
	if counter < 0 {
		return false, errors.New("TOTP counter must be non-negative")
	}
	if len(backupCodeDigests) != auth.DefaultMFABackupCodeCount {
		return false, errors.New("TOTP rotation requires the default MFA backup code set")
	}
	confirmed := false
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		if locked, err := r.lockUser(ctx, tx, subjectID, false); err != nil || !locked {
			return err
		}
		ciphertext, err := r.lockPending(ctx, tx, subjectID, secretDigest, true)
		if err != nil || ciphertext == nil {
			return err
		}
		if err := r.spendStepUp(ctx, tx, subjectID, stepUp); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, r.q(`
			UPDATE forge_totp_factors
			SET secret_digest = ?,
			    secret_ciphertext = ?,
			    last_counter = ?,
			    updated_at = {now}
			WHERE user_id = ?
		`), secretDigest[:], ciphertext, counter, subjectID)
		rotated, err := affectedOne(result, err, r.translate)
		if err != nil {
			return err
		}
		if !rotated {
			return errors.New("active TOTP factor disappeared while rotating")
		}
		if err := r.replaceBackupCodes(ctx, tx, subjectID, backupCodeDigests); err != nil {
			return err
		}
		if err := r.endCredentialVersion(ctx, tx, subjectID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, r.q(`DELETE FROM forge_totp_enrollments WHERE user_id = ?`), subjectID); err != nil {
			return r.translate(err)
		}
		confirmed = true
		return nil
	})
	return confirmed, err
}

// DisableMFA removes the account's factor, pending enrollment and backup
// codes, spending stepUp when given. It reports false when the account had no
// active factor.
func (r *Repository) DisableMFA(ctx context.Context, subjectID uuid.UUID, stepUp *auth.Digest) (bool, error) {
	if !isV7(subjectID) {
		return false, errors.New("MFA subject ID must be a UUIDv7")
	}
	disabled := false
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		locked, err := r.lockActiveFactorUser(ctx, tx, subjectID)
		if err != nil || !locked {
			return err
		}
		if err := r.spendStepUp(ctx, tx, subjectID, stepUp); err != nil {
			return err
		}
		for _, table := range []string{"forge_mfa_backup_codes", "forge_totp_enrollments", "forge_totp_factors"} {
			// The table names are constants.
			if _, err := tx.ExecContext(ctx, r.q(`DELETE FROM `+table+` WHERE user_id = ?`), subjectID); err != nil {
				return r.translate(err)
			}
		}
		if err := r.endCredentialVersion(ctx, tx, subjectID); err != nil {
			return err
		}
		disabled = true
		return nil
	})
	return disabled, err
}

// PruneMFA deletes expired enrollment rows and consumed or expired login
// challenges in bounded batches. Each category is capped at limit.
func (r *Repository) PruneMFA(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, errors.New("prune limit must be positive")
	}
	enrollments, err := r.pruneByID(ctx, `
		SELECT user_id
		FROM forge_totp_enrollments
		WHERE expires_at <= {now}
		ORDER BY expires_at
		LIMIT ?{skip_locked}
	`, `DELETE FROM forge_totp_enrollments WHERE user_id = ?`, limit)
	if err != nil {
		return 0, err
	}
	challenges, err := r.pruneByID(ctx, `
		SELECT id
		FROM forge_mfa_challenges
		WHERE consumed_at IS NOT NULL
		   OR expires_at <= {now}
		   OR session_expires_at <= {now}
		ORDER BY COALESCE(consumed_at, expires_at)
		LIMIT ?{skip_locked}
	`, `DELETE FROM forge_mfa_challenges WHERE id = ?`, limit)
	return enrollments + challenges, err
}

// authDigest converts a stored digest column.
func authDigest(raw []byte) (auth.Digest, error) {
	if len(raw) != len(auth.Digest{}) {
		return auth.Digest{}, errors.New("identity store contains an invalid digest")
	}
	var digest auth.Digest
	copy(digest[:], raw)
	return digest, nil
}
