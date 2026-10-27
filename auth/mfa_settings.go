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
	// HasMFABackupCode reports whether codeDigest is an unused backup code
	// of an active account with a factor, without using it.
	HasMFABackupCode(ctx context.Context, subjectID uuid.UUID, codeDigest Digest) (unused bool, err error)
	// ReplaceMFABackupCodes takes stepUp as MFALifecycleStore's methods do.
	ReplaceMFABackupCodes(ctx context.Context, subjectID uuid.UUID, codeDigests []Digest, stepUp *Digest) (replaced bool, err error)
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

// PendingTOTP returns the key of subjectID's unconfirmed rotation (rotation
// true) or enrollment, for showing it again after a wrong confirmation code.
// found is false when there is none, it expired or the pending key is of the
// other kind (a rotation's while the account has a factor, an enrollment's
// otherwise). Callers must have re-authenticated the account first: the key
// is a credential.
func (s *MFAService) PendingTOTP(ctx context.Context, subjectID uuid.UUID, issuer, account string, rotation bool) (TOTPEnrollment, bool, error) {
	issuer = strings.TrimSpace(issuer)
	account = strings.TrimSpace(account)
	if !validTOTPLabel(issuer, 128) || !validTOTPLabel(account, 320) {
		return TOTPEnrollment{}, false, errors.New("TOTP issuer and account labels are required and must not contain control characters")
	}
	record, found, err := s.store.LoadPendingTOTP(ctx, subjectID)
	if err != nil {
		return TOTPEnrollment{}, false, err
	}
	if !found || record.FactorActive != rotation || record.ExpiresAt.IsZero() || !s.now().UTC().Before(record.ExpiresAt) {
		return TOTPEnrollment{}, false, nil
	}
	secret, err := s.openTOTPSecret(record.SecretCiphertext, record.SecretDigest)
	if err != nil {
		return TOTPEnrollment{}, false, err
	}
	return newTOTPEnrollment(secret, issuer, account, record.ExpiresAt.UTC()), true, nil
}

// MFAStepUpProof is a current code VerifyMFAStepUp accepted for one account.
// The changes of an existing factor (BeginTOTPRotation, ConfirmTOTPRotation,
// RegenerateMFABackupCodes, DisableMFA) require one and refuse a proof of
// another account, or the zero value, with ErrMFAStepUpInvalid. A backup code
// is used up only by the change, in the transaction that applies it. Only
// this package creates proofs: VerifyMFAStepUp, after a current code of the
// account, and AuthorizeMFAChange, for the signed-in principal of the request
// context only (authhttp.Middleware is the only writer of it) once the
// application's MFAChangeAuthorizer passed; the JSON two-factor handlers of
// authhttp use the latter.
type MFAStepUpProof struct {
	subjectID  uuid.UUID
	backupCode *Digest
}

// BackupCode reports whether the proof is a backup code (still unused until
// the change it authorizes succeeds).
func (p MFAStepUpProof) BackupCode() bool { return p.backupCode != nil }

// Subject is the account the proof was given for.
func (p MFAStepUpProof) Subject() uuid.UUID { return p.subjectID }

// authorize binds a change of subjectID's factor to the proof:
// ErrMFAStepUpInvalid for the zero value or a proof of another account.
func (p MFAStepUpProof) authorize(subjectID uuid.UUID) (mfaChange, error) {
	if p.subjectID == (uuid.UUID{}) || p.subjectID != subjectID {
		return mfaChange{}, ErrMFAStepUpInvalid
	}
	return mfaChange{stepUp: &p}, nil
}

// changeAuthorizedBy is the proof of a change an MFAChangeAuthorizer allowed
// (the JSON handlers' step-up): it carries no backup code.
func changeAuthorizedBy(subjectID uuid.UUID) MFAStepUpProof {
	return MFAStepUpProof{subjectID: subjectID}
}

type mfaChange struct {
	stepUp *MFAStepUpProof
}

func (c mfaChange) backupCode() *Digest {
	if c.stepUp == nil || c.stepUp.backupCode == nil {
		return nil
	}
	digest := *c.stepUp.backupCode
	return &digest
}

// recordStepUpBackupCodeUsed audits the step-up backup code a committed
// change used up.
func (s *MFAService) recordStepUpBackupCodeUsed(ctx context.Context, subjectID uuid.UUID, change mfaChange) error {
	if change.backupCode() == nil {
		return nil
	}
	return s.recordMFAEvent(ctx, SecurityEvent{
		Kind:      SecurityMFABackupCodeUsed,
		Outcome:   SecurityOutcomeSucceeded,
		ActorID:   subjectID,
		SubjectID: subjectID,
	}, true)
}

// VerifyMFAStepUp checks a current code of subjectID's active factor for
// step-up authentication before a factor change: a TOTP code is used up at
// once (its period is recorded), an unused backup code is only checked. Pass
// the proof to the change, which uses the backup code up
// in the transaction that applies the change, so a change that fails costs no
// backup code. A wrong, replayed or used code, and an account without a
// factor, are ErrMFAInvalid. Callers throttle attempts.
func (s *MFAService) VerifyMFAStepUp(ctx context.Context, subjectID uuid.UUID, code string) (MFAStepUpProof, error) {
	store, err := s.settingsStore()
	if err != nil {
		return MFAStepUpProof{}, err
	}
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return MFAStepUpProof{}, errors.New("MFA subject ID must be a UUIDv7")
	}
	if backupDigest, backup := parseMFABackupCode(code); backup {
		unused, err := store.HasMFABackupCode(ctx, subjectID, backupDigest)
		if err != nil {
			return MFAStepUpProof{}, err
		}
		if !unused {
			return MFAStepUpProof{}, ErrMFAInvalid
		}
		return MFAStepUpProof{subjectID: subjectID, backupCode: &backupDigest}, nil
	}
	if err := s.useTOTPCode(ctx, store, subjectID, code); err != nil {
		return MFAStepUpProof{}, err
	}
	return MFAStepUpProof{subjectID: subjectID}, nil
}

// VerifyMFACode checks a current code of subjectID's active factor outside
// sign-in, for step-up authentication: a TOTP code of the current period (or
// one before or after) later than the last accepted one, or an unused backup
// code. The code is used up either way, so it cannot be replayed for sign-in
// or another step-up. A wrong, replayed or used code, and an account without
// a factor, are ErrMFAInvalid. Callers throttle attempts. Before a factor
// change use VerifyMFAStepUp instead, so a change that fails keeps a backup
// code.
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
	return s.useTOTPCode(ctx, store, subjectID, code)
}

// useTOTPCode checks a TOTP code of the active factor and records its period.
func (s *MFAService) useTOTPCode(ctx context.Context, store MFASettingsStore, subjectID uuid.UUID, code string) error {
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
// proof is the step-up of subjectID; its backup code is used up only with
// the replacement. A proof of another account is ErrMFAStepUpInvalid.
func (s *MFAService) RegenerateMFABackupCodes(ctx context.Context, subjectID uuid.UUID, proof MFAStepUpProof) ([]MFABackupCode, error) {
	change, err := proof.authorize(subjectID)
	if err != nil {
		return nil, err
	}
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
	replaced, err := store.ReplaceMFABackupCodes(ctx, subjectID, digests, change.backupCode())
	if err != nil {
		return nil, err
	}
	if !replaced {
		return nil, ErrMFANotEnabled
	}
	return codes, errors.Join(s.recordStepUpBackupCodeUsed(ctx, subjectID, change), s.recordMFAEvent(ctx, SecurityEvent{
		Kind:      SecurityMFABackupCodesRegenerated,
		Outcome:   SecurityOutcomeSucceeded,
		ActorID:   subjectID,
		SubjectID: subjectID,
	}, true))
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
// subjectID. An unknown account still costs one password hash. The caller
// audits a failure and then pads it to the hasher's legacy timing floor.
func verifySubjectPassword(ctx context.Context, hasher *PasswordHasher, store SubjectPasswordStore, subjectID uuid.UUID, password string) (login string, ok bool, err error) {
	if password == "" || len(password) > maxPasswordBytes {
		return "", false, nil
	}
	credential, found, err := store.LookupSubjectPassword(ctx, subjectID)
	if err != nil {
		return "", false, err
	}
	if !found || credential.SubjectID != subjectID {
		if err := hasher.verifyDummy(ctx, password); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
	match, _, err := hasher.Verify(ctx, credential.PasswordHash, password)
	if err != nil || !match {
		return "", false, err
	}
	return credential.Login, true, nil
}
