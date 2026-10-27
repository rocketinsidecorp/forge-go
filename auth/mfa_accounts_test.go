package auth

import (
	"context"
	"encoding/base32"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// mfaAccount is one account of memoryMFAAccounts.
type mfaAccount struct {
	login       string
	hash        string
	pending     *TOTPSecretRecord
	factor      *TOTPSecretRecord
	backupCodes map[Digest]bool // digest -> used
	revocations int
}

// memoryMFAAccounts implements the MFA stores and SubjectPasswordStore for
// several accounts with the semantics auth/postgres has.
type memoryMFAAccounts struct {
	mu       sync.Mutex
	accounts map[uuid.UUID]*mfaAccount
}

func (m *memoryMFAAccounts) account(subjectID uuid.UUID) *mfaAccount { return m.accounts[subjectID] }

func (m *memoryMFAAccounts) LookupSubjectPassword(_ context.Context, subjectID uuid.UUID) (SubjectPassword, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil {
		return SubjectPassword{}, false, nil
	}
	return SubjectPassword{SubjectID: subjectID, Login: account.login, PasswordHash: account.hash}, true, nil
}

// spend uses up stepUp, as auth/postgres does in the change's transaction;
// call it with the lock held, once every other precondition holds.
func (a *mfaAccount) spend(stepUp *Digest) error {
	if stepUp == nil {
		return nil
	}
	if used, exists := a.backupCodes[*stepUp]; !exists || used {
		return ErrMFAStepUpInvalid
	}
	a.backupCodes[*stepUp] = true
	return nil
}

func (m *memoryMFAAccounts) begin(subjectID uuid.UUID, digest Digest, ciphertext []byte, expiresAt time.Time, rotation bool, stepUp *Digest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil {
		return errors.New("no such account")
	}
	if rotation && account.factor == nil {
		return ErrMFANotEnabled
	}
	if !rotation && account.factor != nil {
		return ErrMFAAlreadyEnabled
	}
	if err := account.spend(stepUp); err != nil {
		return err
	}
	account.pending = &TOTPSecretRecord{SubjectID: subjectID, SecretDigest: digest, SecretCiphertext: ciphertext, ExpiresAt: expiresAt}
	return nil
}

func (m *memoryMFAAccounts) BeginTOTPEnrollment(_ context.Context, subjectID uuid.UUID, digest Digest, ciphertext []byte, expiresAt time.Time) error {
	return m.begin(subjectID, digest, ciphertext, expiresAt, false, nil)
}

func (m *memoryMFAAccounts) BeginTOTPRotation(_ context.Context, subjectID uuid.UUID, digest Digest, ciphertext []byte, expiresAt time.Time, stepUp *Digest) error {
	return m.begin(subjectID, digest, ciphertext, expiresAt, true, stepUp)
}

func (m *memoryMFAAccounts) LoadPendingTOTP(_ context.Context, subjectID uuid.UUID) (TOTPSecretRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.pending == nil {
		return TOTPSecretRecord{}, false, nil
	}
	record := *account.pending
	record.FactorActive = account.factor != nil
	return record, true, nil
}

func (m *memoryMFAAccounts) confirm(subjectID uuid.UUID, digest Digest, counter int64, backup []Digest, rotation bool, stepUp *Digest) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.pending == nil || account.pending.SecretDigest != digest || (account.factor != nil) != rotation {
		return false, nil
	}
	if err := account.spend(stepUp); err != nil {
		return false, err
	}
	factor := *account.pending
	factor.LastCounter = counter
	account.factor, account.pending = &factor, nil
	account.backupCodes = map[Digest]bool{}
	for _, code := range backup {
		account.backupCodes[code] = false
	}
	account.revocations++
	return true, nil
}

func (m *memoryMFAAccounts) ConfirmTOTPEnrollment(_ context.Context, subjectID uuid.UUID, digest Digest, counter int64, backup []Digest) (bool, error) {
	return m.confirm(subjectID, digest, counter, backup, false, nil)
}

func (m *memoryMFAAccounts) ConfirmTOTPRotation(_ context.Context, subjectID uuid.UUID, digest Digest, counter int64, backup []Digest, stepUp *Digest) (bool, error) {
	return m.confirm(subjectID, digest, counter, backup, true, stepUp)
}

func (m *memoryMFAAccounts) DisableMFA(_ context.Context, subjectID uuid.UUID, stepUp *Digest) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil {
		return false, nil
	}
	if err := account.spend(stepUp); err != nil {
		return false, err
	}
	account.factor, account.pending, account.backupCodes = nil, nil, nil
	account.revocations++
	return true, nil
}

func (m *memoryMFAAccounts) LoadMFAStatus(_ context.Context, subjectID uuid.UUID) (MFAStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil {
		return MFAStatus{}, nil
	}
	status := MFAStatus{Enabled: true}
	for _, used := range account.backupCodes {
		if !used {
			status.BackupCodesRemaining++
		}
	}
	return status, nil
}

func (m *memoryMFAAccounts) LoadActiveTOTP(_ context.Context, subjectID uuid.UUID) (TOTPSecretRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil {
		return TOTPSecretRecord{}, false, nil
	}
	return *account.factor, true, nil
}

func (m *memoryMFAAccounts) AdvanceTOTPCounter(_ context.Context, subjectID uuid.UUID, digest Digest, counter int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil || account.factor.SecretDigest != digest || counter <= account.factor.LastCounter {
		return false, nil
	}
	account.factor.LastCounter = counter
	return true, nil
}

func (m *memoryMFAAccounts) ConsumeMFABackupCode(_ context.Context, subjectID uuid.UUID, digest Digest) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil {
		return false, nil
	}
	used, exists := account.backupCodes[digest]
	if !exists || used {
		return false, nil
	}
	account.backupCodes[digest] = true
	return true, nil
}

func (m *memoryMFAAccounts) HasMFABackupCode(_ context.Context, subjectID uuid.UUID, digest Digest) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil {
		return false, nil
	}
	used, exists := account.backupCodes[digest]
	return exists && !used, nil
}

func (m *memoryMFAAccounts) ReplaceMFABackupCodes(_ context.Context, subjectID uuid.UUID, digests []Digest, stepUp *Digest) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(subjectID)
	if account == nil || account.factor == nil {
		return false, nil
	}
	if err := account.spend(stepUp); err != nil {
		return false, err
	}
	account.backupCodes = map[Digest]bool{}
	for _, digest := range digests {
		account.backupCodes[digest] = false
	}
	return true, nil
}

const mfaTestPassword = "correct horse battery staple"

func mustBackupDigest(t *testing.T, code string) Digest {
	t.Helper()
	digest, ok := parseMFABackupCode(code)
	if !ok {
		t.Fatalf("not a backup code: %q", code)
	}
	return digest
}

func TestTOTPCodeAtMatchesTheVerifier(t *testing.T) {
	t.Parallel()
	secret := []byte("12345678901234567890")
	key := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	at := time.Unix(59, 0)
	code, err := TOTPCodeAt(strings.ToLower(key), at)
	if err != nil || code != "287082" {
		t.Fatalf("code=%q err=%v, want RFC 6238's 287082", code, err)
	}
	if _, ok := matchTOTPCounter(secret, code, at, -1); !ok {
		t.Fatal("the verifier rejects TOTPCodeAt's code")
	}
	for _, bad := range []string{"", "not base32!", "MFRGG"} {
		if _, err := TOTPCodeAt(bad, at); err == nil {
			t.Errorf("TOTPCodeAt(%q) succeeded", bad)
		}
	}
	if _, err := TOTPCodeAt(key, time.Unix(-60, 0)); err == nil {
		t.Error("a time before 1970 was accepted")
	}
}

func hasEvent(auditor *securityAuditorStub, kind SecurityEventKind, outcome SecurityOutcome) bool {
	for _, event := range auditor.events {
		if event.Kind == kind && event.Outcome == outcome {
			return true
		}
	}
	return false
}

func (*memoryMFAAccounts) CreateMFAChallenge(context.Context, Digest, uuid.UUID, int64, time.Time, time.Time) error {
	return errors.New("not used")
}

func (*memoryMFAAccounts) LoadMFAChallenge(context.Context, Digest) (MFAChallengeRecord, bool, error) {
	return MFAChallengeRecord{}, false, errors.New("not used")
}

func (*memoryMFAAccounts) ConsumeMFAChallenge(context.Context, Digest, Digest, int64) (uuid.UUID, bool, error) {
	return uuid.UUID{}, false, errors.New("not used")
}

func (*memoryMFAAccounts) ConsumeMFABackupChallenge(context.Context, Digest, Digest) (uuid.UUID, bool, error) {
	return uuid.UUID{}, false, errors.New("not used")
}
