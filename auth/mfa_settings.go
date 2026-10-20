package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// MFAStatus is the second-factor state of one account, for its settings page.
type MFAStatus struct {
	// Enabled is true while the account has an active TOTP factor.
	Enabled bool
	// BackupCodesRemaining counts the unused backup codes.
	BackupCodesRemaining int
}

// MFASettingsStore is the persistence an account's own two-factor settings
// need beyond MFAStore and MFALifecycleStore: its status, checking a current
// code outside sign-in (step-up) and replacing the backup codes.
//
// AdvanceTOTPCounter must accept counter only when it is greater than the
// stored one for the factor whose seed digest is secretDigest, atomically, so
// one TOTP period is never accepted twice (by sign-in or step-up).
// ConsumeMFABackupCode must mark one unused code of an account with an active
// factor used, atomically. ReplaceMFABackupCodes must replace every backup
// code of an account with an active factor in one transaction and report
// false when the account has none.
type MFASettingsStore interface {
	LoadMFAStatus(ctx context.Context, subjectID uuid.UUID) (MFAStatus, error)
	LoadActiveTOTP(ctx context.Context, subjectID uuid.UUID) (record TOTPSecretRecord, found bool, err error)
	AdvanceTOTPCounter(ctx context.Context, subjectID uuid.UUID, secretDigest Digest, counter int64) (advanced bool, err error)
	ConsumeMFABackupCode(ctx context.Context, subjectID uuid.UUID, codeDigest Digest) (consumed bool, err error)
	ReplaceMFABackupCodes(ctx context.Context, subjectID uuid.UUID, codeDigests []Digest) (replaced bool, err error)
}

// SubjectPassword is the password credential of a signed-in account, for
// step-up authentication. PasswordHash must never reach a client or a log.
type SubjectPassword struct {
	SubjectID    uuid.UUID
	Login        string
	PasswordHash string
}

// SubjectPasswordStore resolves the password credential of an active account
// by its subject ID, so a signed-in user can prove the password again before
// changing a second factor.
type SubjectPasswordStore interface {
	LookupSubjectPassword(ctx context.Context, subjectID uuid.UUID) (credential SubjectPassword, found bool, err error)
}

// MFAStatus reports whether subjectID has a factor and how many backup codes
// remain.
func (s *MFAService) MFAStatus(ctx context.Context, subjectID uuid.UUID) (MFAStatus, error) {
	store, err := s.settingsStore()
	if err != nil {
		return MFAStatus{}, err
	}
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return MFAStatus{}, errors.New("MFA subject ID must be a UUIDv7")
	}
	return store.LoadMFAStatus(ctx, subjectID)
}

// PendingTOTP returns the key of subjectID's unconfirmed enrollment or
// rotation, for showing it again after a wrong confirmation code. found is
// false when there is none or it expired. Callers must have re-authenticated
// the account first: the key is a credential.
func (s *MFAService) PendingTOTP(ctx context.Context, subjectID uuid.UUID, issuer, account string) (TOTPEnrollment, bool, error) {
	issuer = strings.TrimSpace(issuer)
	account = strings.TrimSpace(account)
	if !validTOTPLabel(issuer, 128) || !validTOTPLabel(account, 320) {
		return TOTPEnrollment{}, false, errors.New("TOTP issuer and account labels are required and must not contain control characters")
	}
	record, found, err := s.store.LoadPendingTOTP(ctx, subjectID)
	if err != nil {
		return TOTPEnrollment{}, false, err
	}
	if !found || record.ExpiresAt.IsZero() || !s.now().UTC().Before(record.ExpiresAt) {
		return TOTPEnrollment{}, false, nil
	}
	secret, err := s.openTOTPSecret(record.SecretCiphertext, record.SecretDigest)
	if err != nil {
		return TOTPEnrollment{}, false, err
	}
	return newTOTPEnrollment(secret, issuer, account, record.ExpiresAt.UTC()), true, nil
}

// VerifyMFACode checks a current code of subjectID's active factor outside
// sign-in, for step-up authentication: a TOTP code of the current period (or
// one before or after) later than the last accepted one, or an unused backup
// code. The code is used up either way, so it cannot be replayed for sign-in
// or another step-up. A wrong, replayed or used code, and an account without
// a factor, are ErrMFAInvalid. Callers throttle attempts.
func (s *MFAService) VerifyMFACode(ctx context.Context, subjectID uuid.UUID, code string) error {
	store, err := s.settingsStore()
	if err != nil {
		return err
	}
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return errors.New("MFA subject ID must be a UUIDv7")
	}
	if backupDigest, backup := parseMFABackupCode(code); backup {
		consumed, err := store.ConsumeMFABackupCode(ctx, subjectID, backupDigest)
		if err != nil {
			return err
		}
		if !consumed {
			return ErrMFAInvalid
		}
		return s.recordMFAEvent(ctx, SecurityEvent{
			Kind:      SecurityMFABackupCodeUsed,
			Outcome:   SecurityOutcomeSucceeded,
			ActorID:   subjectID,
			SubjectID: subjectID,
		}, true)
	}
	record, found, err := store.LoadActiveTOTP(ctx, subjectID)
	if err != nil {
		return err
	}
	if !found {
		return ErrMFAInvalid
	}
	secret, err := s.openTOTPSecret(record.SecretCiphertext, record.SecretDigest)
	if err != nil {
		return err
	}
	counter, ok := matchTOTPCounter(secret, code, s.now().UTC(), record.LastCounter)
	if !ok {
		return ErrMFAInvalid
	}
	advanced, err := store.AdvanceTOTPCounter(ctx, subjectID, record.SecretDigest, counter)
	if err != nil {
		return err
	}
	if !advanced {
		return ErrMFAInvalid
	}
	return nil
}

// RegenerateMFABackupCodes replaces every backup code of subjectID with
// DefaultMFABackupCodeCount new ones and returns them; show them once. The
// old codes stop working. An account without a factor is ErrMFANotEnabled.
// Callers must have re-authenticated the account (password and a current
// code). Like confirmation, an audit failure after the codes were replaced
// is returned with the codes as a *SecurityAuditError with OperationApplied.
func (s *MFAService) RegenerateMFABackupCodes(ctx context.Context, subjectID uuid.UUID) ([]MFABackupCode, error) {
	store, err := s.settingsStore()
	if err != nil {
		return nil, err
	}
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return nil, errors.New("MFA subject ID must be a UUIDv7")
	}
	codes, digests, err := newMFABackupCodes(DefaultMFABackupCodeCount)
	if err != nil {
		return nil, err
	}
	replaced, err := store.ReplaceMFABackupCodes(ctx, subjectID, digests)
	if err != nil {
		return nil, err
	}
	if !replaced {
		return nil, ErrMFANotEnabled
	}
	return codes, s.recordMFAEvent(ctx, SecurityEvent{
		Kind:      SecurityMFABackupCodesRegenerated,
		Outcome:   SecurityOutcomeSucceeded,
		ActorID:   subjectID,
		SubjectID: subjectID,
	}, true)
}

func (s *MFAService) settingsStore() (MFASettingsStore, error) {
	store, ok := s.store.(MFASettingsStore)
	if !ok {
		return nil, errors.New("MFA settings are not supported by the configured store")
	}
	return store, nil
}

// deriveMFAStepUpThrottleKey counts step-up attempts per account and per
// source on the MFA throttler, apart from sign-in challenges.
func deriveMFAStepUpThrottleKey(subjectID uuid.UUID, source string) LoginThrottleKey {
	return newThrottleKey("mfa-step-up", subjectID.String(), source, false)
}

// verifySubjectPassword checks password against the stored credential of
// subjectID. An unknown account still costs one password hash.
func verifySubjectPassword(ctx context.Context, store SubjectPasswordStore, subjectID uuid.UUID, password string) (login string, ok bool, err error) {
	if password == "" || len(password) > maxPasswordBytes {
		return "", false, nil
	}
	credential, found, err := store.LookupSubjectPassword(ctx, subjectID)
	if err != nil {
		return "", false, err
	}
	if !found || credential.SubjectID != subjectID {
		if _, _, err := VerifyPassword(dummyPasswordHash, password); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
	match, _, err := VerifyPassword(credential.PasswordHash, password)
	if err != nil || !match {
		return "", false, err
	}
	return credential.Login, true, nil
}
