package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

var errInjected = errors.New("injected store failure")

// A password hash costs 600,000 PBKDF2 rounds; the fixtures share one.
var mfaTestPasswordHash = sync.OnceValues(func() (string, error) { return HashPassword(mfaTestPassword) })

func cachedPasswordHash(t *testing.T) string {
	t.Helper()
	hash, err := mfaTestPasswordHash()
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// cachedIdentity is knownIdentity for mfaTestPassword without hashing again.
func cachedIdentity(t *testing.T, mfa bool) *loginStoreStub {
	t.Helper()
	return &loginStoreStub{found: true, identity: PasswordIdentity{
		SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: cachedPasswordHash(t), CredentialVersion: 1, MFARequired: mfa,
	}}
}

// faultMFAStore wraps the in-memory MFA accounts and fails the named store
// methods, so every error branch of MFAService is reachable. The challenge
// methods, which the memory accounts do not implement, answer from fields.
type faultMFAStore struct {
	*memoryMFAAccounts
	fail map[string]error
	// deny makes the named boolean-result methods answer "not applied".
	deny map[string]bool

	// before runs at the start of the named method, so a test can change the
	// world between two calls of one operation.
	before map[string]func()

	challenge      MFAChallengeRecord
	challengeFound bool
	membership     uuid.UUID
}

func (s *faultMFAStore) failed(method string) error {
	if hook := s.before[method]; hook != nil {
		hook()
	}
	return s.fail[method]
}

func (s *faultMFAStore) LookupSubjectPassword(ctx context.Context, subject uuid.UUID) (SubjectPassword, bool, error) {
	if err := s.failed("LookupSubjectPassword"); err != nil {
		return SubjectPassword{}, false, err
	}
	return s.memoryMFAAccounts.LookupSubjectPassword(ctx, subject)
}

func (s *faultMFAStore) BeginTOTPEnrollment(ctx context.Context, subject uuid.UUID, digest Digest, ciphertext []byte, expiresAt time.Time) error {
	if err := s.failed("BeginTOTPEnrollment"); err != nil {
		return err
	}
	return s.memoryMFAAccounts.BeginTOTPEnrollment(ctx, subject, digest, ciphertext, expiresAt)
}

func (s *faultMFAStore) BeginTOTPRotation(ctx context.Context, subject uuid.UUID, digest Digest, ciphertext []byte, expiresAt time.Time, stepUp *Digest) error {
	if err := s.failed("BeginTOTPRotation"); err != nil {
		return err
	}
	return s.memoryMFAAccounts.BeginTOTPRotation(ctx, subject, digest, ciphertext, expiresAt, stepUp)
}

func (s *faultMFAStore) LoadPendingTOTP(ctx context.Context, subject uuid.UUID) (TOTPSecretRecord, bool, error) {
	if err := s.failed("LoadPendingTOTP"); err != nil {
		return TOTPSecretRecord{}, false, err
	}
	return s.memoryMFAAccounts.LoadPendingTOTP(ctx, subject)
}

func (s *faultMFAStore) ConfirmTOTPEnrollment(ctx context.Context, subject uuid.UUID, digest Digest, counter int64, backup []Digest) (bool, error) {
	if err := s.failed("ConfirmTOTPEnrollment"); err != nil {
		return false, err
	}
	if s.deny["ConfirmTOTPEnrollment"] {
		return false, nil
	}
	return s.memoryMFAAccounts.ConfirmTOTPEnrollment(ctx, subject, digest, counter, backup)
}

func (s *faultMFAStore) ConfirmTOTPRotation(ctx context.Context, subject uuid.UUID, digest Digest, counter int64, backup []Digest, stepUp *Digest) (bool, error) {
	if err := s.failed("ConfirmTOTPRotation"); err != nil {
		return false, err
	}
	if s.deny["ConfirmTOTPRotation"] {
		return false, nil
	}
	return s.memoryMFAAccounts.ConfirmTOTPRotation(ctx, subject, digest, counter, backup, stepUp)
}

func (s *faultMFAStore) DisableMFA(ctx context.Context, subject uuid.UUID, stepUp *Digest) (bool, error) {
	if err := s.failed("DisableMFA"); err != nil {
		return false, err
	}
	return s.memoryMFAAccounts.DisableMFA(ctx, subject, stepUp)
}

func (s *faultMFAStore) LoadMFAStatus(ctx context.Context, subject uuid.UUID) (MFAStatus, error) {
	if err := s.failed("LoadMFAStatus"); err != nil {
		return MFAStatus{}, err
	}
	return s.memoryMFAAccounts.LoadMFAStatus(ctx, subject)
}

func (s *faultMFAStore) LoadActiveTOTP(ctx context.Context, subject uuid.UUID) (TOTPSecretRecord, bool, error) {
	if err := s.failed("LoadActiveTOTP"); err != nil {
		return TOTPSecretRecord{}, false, err
	}
	return s.memoryMFAAccounts.LoadActiveTOTP(ctx, subject)
}

func (s *faultMFAStore) AdvanceTOTPCounter(ctx context.Context, subject uuid.UUID, digest Digest, counter int64) (bool, error) {
	if err := s.failed("AdvanceTOTPCounter"); err != nil {
		return false, err
	}
	if s.deny["AdvanceTOTPCounter"] {
		return false, nil
	}
	return s.memoryMFAAccounts.AdvanceTOTPCounter(ctx, subject, digest, counter)
}

func (s *faultMFAStore) ConsumeMFABackupCode(ctx context.Context, subject uuid.UUID, digest Digest) (bool, error) {
	if err := s.failed("ConsumeMFABackupCode"); err != nil {
		return false, err
	}
	return s.memoryMFAAccounts.ConsumeMFABackupCode(ctx, subject, digest)
}

func (s *faultMFAStore) HasMFABackupCode(ctx context.Context, subject uuid.UUID, digest Digest) (bool, error) {
	if err := s.failed("HasMFABackupCode"); err != nil {
		return false, err
	}
	return s.memoryMFAAccounts.HasMFABackupCode(ctx, subject, digest)
}

func (s *faultMFAStore) ReplaceMFABackupCodes(ctx context.Context, subject uuid.UUID, digests []Digest, stepUp *Digest) (bool, error) {
	if err := s.failed("ReplaceMFABackupCodes"); err != nil {
		return false, err
	}
	return s.memoryMFAAccounts.ReplaceMFABackupCodes(ctx, subject, digests, stepUp)
}

func (s *faultMFAStore) CreateMFAChallenge(context.Context, Digest, uuid.UUID, int64, time.Time, time.Time) error {
	return s.failed("CreateMFAChallenge")
}

func (s *faultMFAStore) LoadMFAChallenge(context.Context, Digest) (MFAChallengeRecord, bool, error) {
	if err := s.failed("LoadMFAChallenge"); err != nil {
		return MFAChallengeRecord{}, false, err
	}
	return s.challenge, s.challengeFound, nil
}

func (s *faultMFAStore) ConsumeMFAChallenge(context.Context, Digest, Digest, int64) (uuid.UUID, bool, error) {
	if err := s.failed("ConsumeMFAChallenge"); err != nil {
		return uuid.UUID{}, false, err
	}
	return s.membership, !s.deny["ConsumeMFAChallenge"], nil
}

func (s *faultMFAStore) ConsumeMFABackupChallenge(context.Context, Digest, Digest) (uuid.UUID, bool, error) {
	if err := s.failed("ConsumeMFABackupChallenge"); err != nil {
		return uuid.UUID{}, false, err
	}
	return s.membership, !s.deny["ConsumeMFABackupChallenge"], nil
}

// faultMFA is one service over a faultMFAStore with an enrolled account.
type faultMFA struct {
	t       *testing.T
	store   *faultMFAStore
	service *MFAService
	subject uuid.UUID
	now     time.Time
	secret  []byte
	audits  []SecurityEvent
	// auditErr, when set, is returned by the auditor after it records.
	auditErr error
	sessions *sessionStub
}

func newFaultMFA(t *testing.T, options ...MFAOption) *faultMFA {
	t.Helper()
	f := &faultMFA{
		t:       t,
		subject: uuid.MustNew(),
		now:     time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
		secret:  []byte("12345678901234567890"),
	}
	hash := cachedPasswordHash(t)
	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	f.sessions = &sessionStub{token: token}
	memory := &memoryMFAAccounts{accounts: map[uuid.UUID]*mfaAccount{
		f.subject: {login: "ada@example.com", hash: hash},
	}}
	f.store = &faultMFAStore{memoryMFAAccounts: memory, fail: map[string]error{}, deny: map[string]bool{}, before: map[string]func(){}, membership: uuid.MustNew()}
	auditor := SecurityAuditorFunc(func(_ context.Context, event SecurityEvent) error {
		f.audits = append(f.audits, event)
		return f.auditErr
	})
	options = append([]MFAOption{WithMFAClock(func() time.Time { return f.now }), WithMFAAuditor(auditor)}, options...)
	f.service, err = NewMFAService(f.store, f.sessions, cipher, options...)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// enroll gives the account an active factor over f.secret and returns its
// sealed secret record.
func (f *faultMFA) enroll() {
	f.t.Helper()
	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		f.t.Fatal(err)
	}
	sealed, err := cipher.Seal(f.secret)
	if err != nil {
		f.t.Fatal(err)
	}
	_, digests, err := newMFABackupCodes(2)
	if err != nil {
		f.t.Fatal(err)
	}
	backup := map[Digest]bool{}
	for _, digest := range digests {
		backup[digest] = false
	}
	account := f.store.accounts[f.subject]
	account.factor = &TOTPSecretRecord{SubjectID: f.subject, SecretDigest: digestOf(f.secret), SecretCiphertext: sealed, LastCounter: -1}
	account.backupCodes = backup
}

func (f *faultMFA) code() string { return totpCode(f.secret, uint64(f.now.Unix()/totpPeriodSeconds)) }

func digestOf(secret []byte) Digest { return sha256.Sum256(secret) }

func (f *faultMFA) expectAudit(kind SecurityEventKind, outcome SecurityOutcome) {
	f.t.Helper()
	for _, event := range f.audits {
		if event.Kind == kind && event.Outcome == outcome {
			return
		}
	}
	f.t.Fatalf("no %s/%s audit event in %+v", kind, outcome, f.audits)
}

func TestMFASecretValuesAreRedactedInEveryFormat(t *testing.T) {
	t.Parallel()
	token, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	enrollment := newTOTPEnrollment([]byte("12345678901234567890"), "Forge", "ada", time.Unix(0, 0))
	values := map[string]struct {
		value  any
		secret string
	}{
		"challenge token": {token, token.Reveal()},
		"totp secret":     {enrollment.Secret, enrollment.Secret.Reveal()},
		"provisioning":    {enrollment.ProvisioningURI, enrollment.ProvisioningURI.Reveal()},
	}
	for name, tc := range values {
		for _, text := range []string{
			fmt.Sprintf("%v", tc.value), fmt.Sprintf("%s", tc.value), fmt.Sprintf("%#v", tc.value),
		} {
			if strings.Contains(text, tc.secret) {
				t.Errorf("%s leaks its secret through fmt: %q", name, text)
			}
			if !strings.Contains(text, "REDACTED") {
				t.Errorf("%s is not marked redacted: %q", name, text)
			}
		}
		valuer, ok := tc.value.(slog.LogValuer)
		if !ok {
			t.Fatalf("%s is not a slog.LogValuer", name)
		}
		if got := valuer.LogValue().String(); got != "[REDACTED]" {
			t.Errorf("%s logs %q", name, got)
		}
	}
}

func TestAESGCMSecretCipherRejectsBadKeysAndUnconfiguredUse(t *testing.T) {
	t.Parallel()
	if _, err := NewAESGCMSecretCipher(make([]byte, 16)); err == nil {
		t.Fatal("a 16-byte key was accepted")
	}
	for name, cipher := range map[string]*AESGCMSecretCipher{"nil": nil, "zero": {}} {
		if _, err := cipher.Seal([]byte("x")); err == nil {
			t.Errorf("%s cipher sealed", name)
		}
		if _, err := cipher.Open([]byte("x")); err == nil {
			t.Errorf("%s cipher opened", name)
		}
	}
}

func TestMFAServiceConstructionValidatesItsOptions(t *testing.T) {
	t.Parallel()
	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	store := &mfaStoreStub{}
	sessions := &sessionStub{}
	if _, err := NewMFAService(nil, sessions, cipher); err == nil {
		t.Error("a nil store was accepted")
	}
	if _, err := NewMFAService(store, nil, cipher); err == nil {
		t.Error("nil sessions were accepted")
	}
	if _, err := NewMFAService(store, sessions, nil); err == nil {
		t.Error("a nil cipher was accepted")
	}
	for name, option := range map[string]MFAOption{
		"nil option":    nil,
		"nil clock":     WithMFAClock(nil),
		"short ttl":     WithMFAChallengeTTL(time.Second),
		"long ttl":      WithMFAChallengeTTL(time.Hour),
		"nil throttler": WithMFAThrottler(nil),
		"nil auditor":   WithMFAAuditor(nil),
	} {
		if _, err := NewMFAService(store, sessions, cipher, option); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	service, err := NewMFAService(store, sessions, cipher, WithMFAChallengeTTL(10*time.Minute))
	if err != nil || service.challengeTTL != 10*time.Minute {
		t.Fatalf("ttl=%v err=%v", service.challengeTTL, err)
	}
}

func TestMFABeginTOTPFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFaultMFA(t)
	if _, err := f.service.BeginTOTPEnrollment(ctx, uuid.UUID{}, "Forge", "ada"); err == nil {
		t.Error("a non-UUIDv7 subject was accepted")
	}
	for _, labels := range [][2]string{{"", "ada"}, {"Forge", ""}, {"For\nge", "ada"}, {"Forge", "a\x00da"}, {strings.Repeat("x", 129), "ada"}} {
		if _, err := f.service.BeginTOTPEnrollment(ctx, f.subject, labels[0], labels[1]); err == nil {
			t.Errorf("labels %q were accepted", labels)
		}
	}
	f.store.fail["BeginTOTPEnrollment"] = errInjected
	if _, err := f.service.BeginTOTPEnrollment(ctx, f.subject, "Forge", "ada"); !errors.Is(err, errInjected) {
		t.Errorf("store failure: %v", err)
	}
	delete(f.store.fail, "BeginTOTPEnrollment")

	f.enroll()
	proof := changeAuthorizedBy(f.subject)
	f.store.fail["BeginTOTPRotation"] = errInjected
	if _, err := f.service.BeginTOTPRotation(ctx, f.subject, "Forge", "ada", proof); !errors.Is(err, errInjected) {
		t.Errorf("rotation store failure: %v", err)
	}
	if _, err := f.service.BeginTOTPRotation(ctx, f.subject, "Forge", "ada", MFAStepUpProof{}); !errors.Is(err, ErrMFAStepUpInvalid) {
		t.Errorf("zero proof: %v", err)
	}

	// An audit failure after the seed was stored is reported with the
	// enrollment and flagged as applied.
	delete(f.store.fail, "BeginTOTPRotation")
	f.auditErr = errors.New("audit down")
	enrollment, err := f.service.BeginTOTPRotation(ctx, f.subject, "Forge", "ada", proof)
	var auditFailure *SecurityAuditError
	if !errors.As(err, &auditFailure) || !auditFailure.OperationApplied || enrollment.Secret.Reveal() == "" {
		t.Fatalf("enrollment=%v err=%v", enrollment, err)
	}
	f.expectAudit(SecurityMFARotationStarted, SecurityOutcomeSucceeded)
}

func TestMFALifecycleNeedsALifecycleStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	subject := uuid.MustNew()
	// Only MFAStore's methods are visible through the embedded interface.
	service, err := NewMFAService(struct{ MFAStore }{&mfaStoreStub{}}, &sessionStub{}, cipher)
	if err != nil {
		t.Fatal(err)
	}
	proof := changeAuthorizedBy(subject)
	if _, err := service.BeginTOTPRotation(ctx, subject, "Forge", "ada", proof); err == nil || !strings.Contains(err.Error(), "lifecycle") {
		t.Errorf("begin rotation: %v", err)
	}
	if err := service.DisableMFA(ctx, subject, proof); err == nil || !strings.Contains(err.Error(), "lifecycle") {
		t.Errorf("disable: %v", err)
	}
	for name, call := range map[string]func() error{
		"status":      func() error { _, err := service.MFAStatus(ctx, subject); return err },
		"step-up":     func() error { _, err := service.VerifyMFAStepUp(ctx, subject, "123456"); return err },
		"verify code": func() error { return service.VerifyMFACode(ctx, subject, "123456") },
		"regenerate":  func() error { _, err := service.RegenerateMFABackupCodes(ctx, subject, proof); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestMFAConfirmFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFaultMFA(t)
	enrollment, err := f.service.BeginTOTPEnrollment(ctx, f.subject, "Forge", "ada")
	if err != nil {
		t.Fatal(err)
	}
	pendingCode, err := TOTPCodeAt(enrollment.Secret.Reveal(), f.now)
	if err != nil {
		t.Fatal(err)
	}

	f.store.fail["LoadPendingTOTP"] = errInjected
	if _, err := f.service.ConfirmTOTPEnrollment(ctx, f.subject, pendingCode); !errors.Is(err, errInjected) {
		t.Errorf("load failure: %v", err)
	}
	delete(f.store.fail, "LoadPendingTOTP")

	// An expired enrollment is refused, and so is a code of another key.
	f.now = f.now.Add(11 * time.Minute)
	if _, err := f.service.ConfirmTOTPEnrollment(ctx, f.subject, pendingCode); !errors.Is(err, ErrMFAEnrollmentInvalid) {
		t.Errorf("expired: %v", err)
	}
	f.now = f.now.Add(-11 * time.Minute)
	if _, err := f.service.ConfirmTOTPEnrollment(ctx, f.subject, "000000"); !errors.Is(err, ErrMFAEnrollmentInvalid) {
		t.Errorf("wrong code: %v", err)
	}

	// A pending record whose ciphertext cannot be opened is an error.
	account := f.store.accounts[f.subject]
	good := account.pending.SecretCiphertext
	account.pending.SecretCiphertext = []byte("garbage")
	if _, err := f.service.ConfirmTOTPEnrollment(ctx, f.subject, pendingCode); err == nil || errors.Is(err, ErrMFAEnrollmentInvalid) {
		t.Errorf("garbage ciphertext: %v", err)
	}
	account.pending.SecretCiphertext = good

	f.store.fail["ConfirmTOTPEnrollment"] = errInjected
	if _, err := f.service.ConfirmTOTPEnrollment(ctx, f.subject, pendingCode); !errors.Is(err, errInjected) {
		t.Errorf("confirm failure: %v", err)
	}
	delete(f.store.fail, "ConfirmTOTPEnrollment")
	f.store.deny["ConfirmTOTPEnrollment"] = true
	if _, err := f.service.ConfirmTOTPEnrollment(ctx, f.subject, pendingCode); !errors.Is(err, ErrMFAEnrollmentInvalid) {
		t.Errorf("not confirmed: %v", err)
	}
	delete(f.store.deny, "ConfirmTOTPEnrollment")

	// An audit failure after the factor was installed returns the codes with
	// an applied audit error.
	f.auditErr = errors.New("audit down")
	confirmation, err := f.service.ConfirmTOTPEnrollment(ctx, f.subject, pendingCode)
	var auditFailure *SecurityAuditError
	if !errors.As(err, &auditFailure) || !auditFailure.OperationApplied || len(confirmation.BackupCodes) != DefaultMFABackupCodeCount {
		t.Fatalf("codes=%d err=%v", len(confirmation.BackupCodes), err)
	}
	f.expectAudit(SecurityMFAEnabled, SecurityOutcomeSucceeded)
}

func TestMFARotationConfirmAndDisableFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFaultMFA(t)
	f.enroll()
	proof := changeAuthorizedBy(f.subject)
	enrollment, err := f.service.BeginTOTPRotation(ctx, f.subject, "Forge", "ada", proof)
	if err != nil {
		t.Fatal(err)
	}
	newCode, err := TOTPCodeAt(enrollment.Secret.Reveal(), f.now)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.service.ConfirmTOTPRotation(ctx, f.subject, newCode, MFAStepUpProof{}); !errors.Is(err, ErrMFAStepUpInvalid) {
		t.Errorf("zero proof: %v", err)
	}
	f.store.fail["LoadPendingTOTP"] = errInjected
	if _, err := f.service.ConfirmTOTPRotation(ctx, f.subject, newCode, proof); !errors.Is(err, errInjected) {
		t.Errorf("load failure: %v", err)
	}
	delete(f.store.fail, "LoadPendingTOTP")
	f.now = f.now.Add(11 * time.Minute)
	if _, err := f.service.ConfirmTOTPRotation(ctx, f.subject, newCode, proof); !errors.Is(err, ErrMFAEnrollmentInvalid) {
		t.Errorf("expired: %v", err)
	}
	f.now = f.now.Add(-11 * time.Minute)
	if _, err := f.service.ConfirmTOTPRotation(ctx, f.subject, "000000", proof); !errors.Is(err, ErrMFAEnrollmentInvalid) {
		t.Errorf("wrong code: %v", err)
	}
	pending := f.store.accounts[f.subject].pending
	good := pending.SecretCiphertext
	pending.SecretCiphertext = []byte("garbage")
	if _, err := f.service.ConfirmTOTPRotation(ctx, f.subject, newCode, proof); err == nil || errors.Is(err, ErrMFAEnrollmentInvalid) {
		t.Errorf("garbage ciphertext: %v", err)
	}
	pending.SecretCiphertext = good
	f.store.fail["ConfirmTOTPRotation"] = errInjected
	if _, err := f.service.ConfirmTOTPRotation(ctx, f.subject, newCode, proof); !errors.Is(err, errInjected) {
		t.Errorf("confirm failure: %v", err)
	}
	delete(f.store.fail, "ConfirmTOTPRotation")
	f.store.deny["ConfirmTOTPRotation"] = true
	if _, err := f.service.ConfirmTOTPRotation(ctx, f.subject, newCode, proof); !errors.Is(err, ErrMFAEnrollmentInvalid) {
		t.Errorf("not confirmed: %v", err)
	}
	delete(f.store.deny, "ConfirmTOTPRotation")
	f.auditErr = errors.New("audit down")
	confirmation, err := f.service.ConfirmTOTPRotation(ctx, f.subject, newCode, proof)
	var auditFailure *SecurityAuditError
	if !errors.As(err, &auditFailure) || !auditFailure.OperationApplied || len(confirmation.BackupCodes) != DefaultMFABackupCodeCount {
		t.Fatalf("codes=%d err=%v", len(confirmation.BackupCodes), err)
	}
	f.expectAudit(SecurityMFARotated, SecurityOutcomeSucceeded)

	// Disable: a proof of nobody, a store failure, an account without factor.
	f.auditErr = nil
	if err := f.service.DisableMFA(ctx, uuid.UUID{}, changeAuthorizedBy(uuid.UUID{})); err == nil {
		t.Error("the zero subject was accepted")
	}
	f.store.fail["DisableMFA"] = errInjected
	if err := f.service.DisableMFA(ctx, f.subject, proof); !errors.Is(err, errInjected) {
		t.Errorf("disable failure: %v", err)
	}
	delete(f.store.fail, "DisableMFA")
	if err := f.service.DisableMFA(ctx, f.subject, proof); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := f.service.DisableMFA(ctx, f.subject, proof); !errors.Is(err, ErrMFANotEnabled) {
		t.Errorf("disable twice: %v", err)
	}
	f.expectAudit(SecurityMFADisabled, SecurityOutcomeSucceeded)
}

func TestMFAIssueChallengeValidatesAndPropagatesStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFaultMFA(t)
	later := f.now.Add(time.Hour)
	if _, _, err := f.service.IssueMFAChallenge(ctx, uuid.UUID{}, 1, later); err == nil {
		t.Error("a non-UUIDv7 membership was accepted")
	}
	if _, _, err := f.service.IssueMFAChallenge(ctx, f.subject, 0, later); err == nil {
		t.Error("credential version 0 was accepted")
	}
	if _, _, err := f.service.IssueMFAChallenge(ctx, f.subject, 1, f.now); err == nil {
		t.Error("an expired session was accepted")
	}
	f.store.fail["CreateMFAChallenge"] = errInjected
	if _, _, err := f.service.IssueMFAChallenge(ctx, f.subject, 1, later); !errors.Is(err, errInjected) {
		t.Errorf("store failure: %v", err)
	}
	delete(f.store.fail, "CreateMFAChallenge")
	token, expiresAt, err := f.service.IssueMFAChallenge(ctx, f.subject, 1, later)
	if err != nil || token.Reveal() == "" || !expiresAt.Equal(f.now.Add(defaultMFAChallengeTTL)) {
		t.Fatalf("token=%v expires=%v err=%v", token, expiresAt, err)
	}
}

// failingThrottler fails or denies the MFA throttle.
type failingThrottler struct {
	attemptErr, successErr error
	allowed                bool
}

func (t failingThrottler) Attempt(context.Context, LoginThrottleKey) (LoginThrottleDecision, error) {
	return LoginThrottleDecision{Allowed: t.allowed, RetryAfter: 7 * time.Second}, t.attemptErr
}

func (t failingThrottler) Success(context.Context, LoginThrottleKey) error { return t.successErr }

// revokingSessions records revocations of sessions created by CompleteLogin.
type revokingSessions struct {
	sessionStub
	revoked   []Token
	createErr error
}

func (s *revokingSessions) CreateSessionAtVersion(ctx context.Context, membership uuid.UUID, version int64, expiresAt time.Time) (Token, error) {
	if s.createErr != nil {
		return Token{}, s.createErr
	}
	return s.sessionStub.CreateSessionAtVersion(ctx, membership, version, expiresAt)
}

func (s *revokingSessions) RevokeSession(_ context.Context, token Token) error {
	s.revoked = append(s.revoked, token)
	return nil
}

func TestMFACompleteLoginFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	challenge, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	throttler, err := NewMemoryLoginThrottler(LoginThrottleConfig{Window: time.Hour, AccountLimit: 1000, SourceLimit: 1000, MaxEntries: 100})
	if err != nil {
		t.Fatal(err)
	}
	f := newFaultMFA(t, WithMFAThrottler(throttler))
	f.store.challengeFound = true
	f.store.challenge = MFAChallengeRecord{
		SubjectID:         f.subject,
		MembershipID:      f.store.membership,
		CredentialVersion: 3,
		SecretDigest:      digestOf(f.secret),
		LastCounter:       -1,
		ExpiresAt:         f.now.Add(time.Minute),
		SessionExpiresAt:  f.now.Add(time.Hour),
	}
	f.enroll()
	sealed := f.store.accounts[f.subject].factor.SecretCiphertext
	f.store.challenge.SecretCiphertext = sealed
	complete := func(code string) error {
		_, err := f.service.CompleteLogin(ctx, MFACompletion{ChallengeToken: challenge.Reveal(), Code: code, Source: "192.0.2.1"})
		return err
	}

	if err := complete("000000"); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("wrong code: %v", err)
	}
	f.expectAudit(SecurityMFAChallengeFailed, SecurityOutcomeDenied)

	// A malformed challenge token never reaches the store.
	if _, err := f.service.CompleteLogin(ctx, MFACompletion{ChallengeToken: "short", Code: f.code()}); !errors.Is(err, ErrMFAInvalid) {
		t.Errorf("malformed challenge: %v", err)
	}

	f.store.fail["LoadMFAChallenge"] = errInjected
	if err := complete(f.code()); !errors.Is(err, errInjected) {
		t.Errorf("load failure: %v", err)
	}
	delete(f.store.fail, "LoadMFAChallenge")

	f.store.fail["ConsumeMFAChallenge"] = errInjected
	if err := complete(f.code()); !errors.Is(err, errInjected) {
		t.Errorf("consume failure: %v", err)
	}
	delete(f.store.fail, "ConsumeMFAChallenge")
	f.store.deny["ConsumeMFAChallenge"] = true
	if err := complete(f.code()); !errors.Is(err, ErrMFAInvalid) {
		t.Errorf("challenge already consumed: %v", err)
	}
	delete(f.store.deny, "ConsumeMFAChallenge")

	codes, _, err := newMFABackupCodes(1)
	if err != nil {
		t.Fatal(err)
	}
	backupCode := codes[0].Reveal()
	f.store.fail["ConsumeMFABackupChallenge"] = errInjected
	if err := complete(backupCode); !errors.Is(err, errInjected) {
		t.Errorf("backup consume failure: %v", err)
	}
	delete(f.store.fail, "ConsumeMFABackupChallenge")

	// A challenge record whose secret does not open is a failure, audited.
	f.store.challenge.SecretCiphertext = []byte("garbage")
	if err := complete(f.code()); err == nil || errors.Is(err, ErrMFAInvalid) {
		t.Errorf("garbage ciphertext: %v", err)
	}
	f.expectAudit(SecurityMFAChallengeFailed, SecurityOutcomeFailed)
	f.store.challenge.SecretCiphertext = sealed

	// A session store failure after the challenge was consumed.
	failing := &revokingSessions{createErr: errInjected}
	f.service.sessions = failing
	if err := complete(f.code()); !errors.Is(err, errInjected) {
		t.Errorf("session failure: %v", err)
	}

	// An audit failure on the success event revokes the new session.
	revoking := &revokingSessions{sessionStub: *f.sessions}
	f.service.sessions = revoking
	f.auditErr = errors.New("audit down")
	f.store.challenge.LastCounter = -1
	if err := complete(f.code()); err == nil {
		t.Error("a login whose audit failed was accepted")
	}
	if len(revoking.revoked) != 1 {
		t.Errorf("revoked sessions = %d, want 1", len(revoking.revoked))
	}
	// Using a backup code audits the code first; its failure also revokes.
	revoking.revoked = nil
	if err := complete(backupCode); err == nil || len(revoking.revoked) != 1 {
		t.Errorf("backup code audit failure: err=%v revoked=%d", err, len(revoking.revoked))
	}
	// Without a revoker the audit error is returned as it is.
	f.service.sessions = f.sessions
	var auditFailure *SecurityAuditError
	if err := complete(f.code()); !errors.As(err, &auditFailure) || auditFailure.OperationApplied {
		t.Errorf("audit failure without a revoker: %v", err)
	}
}

func TestMFACompleteLoginThrottleFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	challenge, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	completion := MFACompletion{ChallengeToken: challenge.Reveal(), Code: "000000"}

	f := newFaultMFA(t, WithMFAThrottler(failingThrottler{attemptErr: errInjected}))
	if _, err := f.service.CompleteLogin(ctx, completion); !errors.Is(err, errInjected) {
		t.Errorf("attempt failure: %v", err)
	}

	f = newFaultMFA(t, WithMFAThrottler(failingThrottler{allowed: false}))
	f.auditErr = errors.New("audit down")
	_, err = f.service.CompleteLogin(ctx, completion)
	var throttled *LoginThrottledError
	if !errors.As(err, &throttled) || throttled.RetryAfter != 7*time.Second || !errors.Is(err, ErrLoginThrottled) {
		t.Errorf("denied attempt: %v", err)
	}
	var auditFailure *SecurityAuditError
	if !errors.As(err, &auditFailure) {
		t.Errorf("the throttled attempt must also report its audit failure: %v", err)
	}
	f.expectAudit(SecurityMFAChallengeFailed, SecurityOutcomeDenied)

	// A success that cannot be recorded in the throttler fails the login
	// before a session exists.
	f = newFaultMFA(t, WithMFAThrottler(failingThrottler{allowed: true, successErr: errInjected}))
	f.enroll()
	f.store.challengeFound = true
	f.store.challenge = MFAChallengeRecord{
		SubjectID: f.subject, MembershipID: f.store.membership, CredentialVersion: 1,
		SecretDigest: digestOf(f.secret), SecretCiphertext: f.store.accounts[f.subject].factor.SecretCiphertext,
		LastCounter: -1, ExpiresAt: f.now.Add(time.Minute), SessionExpiresAt: f.now.Add(time.Hour),
	}
	completion.Code = f.code()
	if _, err := f.service.CompleteLogin(ctx, completion); !errors.Is(err, errInjected) {
		t.Errorf("throttler success failure: %v", err)
	}
	if f.sessions.membership != (uuid.UUID{}) {
		t.Error("a session was created although the throttler failed")
	}
}

func TestMFAOpenTOTPSecretRejectsMalformedSecrets(t *testing.T) {
	t.Parallel()
	f := newFaultMFA(t)
	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	short, err := cipher.Seal([]byte("too short"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.openTOTPSecret(short, digestOf([]byte("too short"))); err == nil || !strings.Contains(err.Error(), "length") {
		t.Errorf("short secret: %v", err)
	}
	sealed, err := cipher.Seal(f.secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.openTOTPSecret(sealed, digestOf([]byte("another secret, 20 b"))); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("digest mismatch: %v", err)
	}
	// Codes are never matched outside the replay window or before 1970.
	if _, ok := matchTOTPCounter(f.secret, f.code(), time.Unix(-100, 0), -1); ok {
		t.Error("a code before 1970 matched")
	}
	if _, ok := matchTOTPCounter(f.secret, "12345", f.now, -1); ok {
		t.Error("a short code matched")
	}
	if _, ok := matchTOTPCounter(f.secret, "12345a", f.now, -1); ok {
		t.Error("a non-digit code matched")
	}
	counter := f.now.Unix() / totpPeriodSeconds
	if _, ok := matchTOTPCounter(f.secret, f.code(), f.now, counter+1); ok {
		t.Error("a replayed counter matched")
	}
	// Right after the epoch the previous period does not exist.
	at := time.Unix(5, 0)
	if _, ok := matchTOTPCounter(f.secret, totpCode(f.secret, 0), at, -1); !ok {
		t.Error("period zero did not match at the epoch")
	}
}

func TestMFASettingsFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFaultMFA(t)
	f.enroll()
	svc := f.service
	if _, err := svc.MFAStatus(ctx, uuid.UUID{}); err == nil {
		t.Error("status of a non-UUIDv7 subject")
	}
	f.store.fail["LoadMFAStatus"] = errInjected
	if _, err := svc.MFAStatus(ctx, f.subject); !errors.Is(err, errInjected) {
		t.Errorf("status failure: %v", err)
	}
	delete(f.store.fail, "LoadMFAStatus")

	// PendingTOTP: bad labels, store failure, unreadable key.
	if _, _, err := svc.PendingTOTP(ctx, f.subject, "", "ada", false); err == nil {
		t.Error("empty issuer accepted")
	}
	f.store.fail["LoadPendingTOTP"] = errInjected
	if _, _, err := svc.PendingTOTP(ctx, f.subject, "Forge", "ada", true); !errors.Is(err, errInjected) {
		t.Errorf("pending failure: %v", err)
	}
	delete(f.store.fail, "LoadPendingTOTP")
	if _, err := svc.BeginTOTPRotation(ctx, f.subject, "Forge", "ada", changeAuthorizedBy(f.subject)); err != nil {
		t.Fatal(err)
	}
	pending := f.store.accounts[f.subject].pending
	good := pending.SecretCiphertext
	pending.SecretCiphertext = []byte("garbage")
	if _, found, err := svc.PendingTOTP(ctx, f.subject, "Forge", "ada", true); err == nil || found {
		t.Errorf("unreadable key: found=%v err=%v", found, err)
	}
	pending.SecretCiphertext = good

	// Step-up: subject, store failures, unknown code kinds.
	if _, err := svc.VerifyMFAStepUp(ctx, uuid.UUID{}, "123456"); err == nil {
		t.Error("step-up of a non-UUIDv7 subject")
	}
	codes, _, err := newMFABackupCodes(1)
	if err != nil {
		t.Fatal(err)
	}
	backup := codes[0].Reveal()
	f.store.fail["HasMFABackupCode"] = errInjected
	if _, err := svc.VerifyMFAStepUp(ctx, f.subject, backup); !errors.Is(err, errInjected) {
		t.Errorf("has-backup failure: %v", err)
	}
	delete(f.store.fail, "HasMFABackupCode")
	f.store.fail["LoadActiveTOTP"] = errInjected
	if _, err := svc.VerifyMFAStepUp(ctx, f.subject, f.code()); !errors.Is(err, errInjected) {
		t.Errorf("active failure: %v", err)
	}
	if err := svc.VerifyMFACode(ctx, f.subject, f.code()); !errors.Is(err, errInjected) {
		t.Errorf("verify code, active failure: %v", err)
	}
	delete(f.store.fail, "LoadActiveTOTP")

	// VerifyMFACode.
	if err := svc.VerifyMFACode(ctx, uuid.UUID{}, "123456"); err == nil {
		t.Error("verify of a non-UUIDv7 subject")
	}
	f.store.fail["ConsumeMFABackupCode"] = errInjected
	if err := svc.VerifyMFACode(ctx, f.subject, backup); !errors.Is(err, errInjected) {
		t.Errorf("consume failure: %v", err)
	}
	delete(f.store.fail, "ConsumeMFABackupCode")
	if err := svc.VerifyMFACode(ctx, f.subject, backup); !errors.Is(err, ErrMFAInvalid) {
		t.Errorf("an unknown backup code: %v", err)
	}
	account := f.store.accounts[f.subject]
	account.backupCodes[mustBackupDigest(t, backup)] = false
	if err := svc.VerifyMFACode(ctx, f.subject, backup); err != nil {
		t.Errorf("an unused backup code: %v", err)
	}
	f.expectAudit(SecurityMFABackupCodeUsed, SecurityOutcomeSucceeded)

	// useTOTPCode: no factor, unreadable secret, counter not advanced.
	f.store.deny["AdvanceTOTPCounter"] = true
	if err := svc.VerifyMFACode(ctx, f.subject, f.code()); !errors.Is(err, ErrMFAInvalid) {
		t.Errorf("replayed period: %v", err)
	}
	delete(f.store.deny, "AdvanceTOTPCounter")
	f.store.fail["AdvanceTOTPCounter"] = errInjected
	if err := svc.VerifyMFACode(ctx, f.subject, f.code()); !errors.Is(err, errInjected) {
		t.Errorf("advance failure: %v", err)
	}
	delete(f.store.fail, "AdvanceTOTPCounter")
	factor := account.factor
	good = factor.SecretCiphertext
	factor.SecretCiphertext = []byte("garbage")
	if err := svc.VerifyMFACode(ctx, f.subject, f.code()); err == nil || errors.Is(err, ErrMFAInvalid) {
		t.Errorf("unreadable factor: %v", err)
	}
	factor.SecretCiphertext = good
	other := uuid.MustNew()
	f.store.accounts[other] = &mfaAccount{login: "bob@example.com"}
	if err := svc.VerifyMFACode(ctx, other, f.code()); !errors.Is(err, ErrMFAInvalid) {
		t.Errorf("an account without a factor: %v", err)
	}
	if _, err := svc.VerifyMFAStepUp(ctx, other, f.code()); !errors.Is(err, ErrMFAInvalid) {
		t.Errorf("step-up of an account without a factor: %v", err)
	}

	// RegenerateMFABackupCodes.
	proof := changeAuthorizedBy(f.subject)
	if _, err := svc.RegenerateMFABackupCodes(ctx, uuid.UUID{}, changeAuthorizedBy(uuid.UUID{})); err == nil {
		t.Error("regenerate for a non-UUIDv7 subject")
	}
	f.store.fail["ReplaceMFABackupCodes"] = errInjected
	if _, err := svc.RegenerateMFABackupCodes(ctx, f.subject, proof); !errors.Is(err, errInjected) {
		t.Errorf("replace failure: %v", err)
	}
	delete(f.store.fail, "ReplaceMFABackupCodes")
	f.store.accounts[other].factor = nil
	if _, err := svc.RegenerateMFABackupCodes(ctx, other, changeAuthorizedBy(other)); !errors.Is(err, ErrMFANotEnabled) {
		t.Errorf("regenerate without a factor: %v", err)
	}
}

func TestVerifySubjectPasswordFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFaultMFA(t)
	if _, ok, err := verifySubjectPassword(ctx, f.store, f.subject, ""); ok || err != nil {
		t.Errorf("empty password: ok=%v err=%v", ok, err)
	}
	if _, ok, err := verifySubjectPassword(ctx, f.store, f.subject, strings.Repeat("x", maxPasswordBytes+1)); ok || err != nil {
		t.Errorf("oversized password: ok=%v err=%v", ok, err)
	}
	f.store.fail["LookupSubjectPassword"] = errInjected
	if _, _, err := verifySubjectPassword(ctx, f.store, f.subject, mfaTestPassword); !errors.Is(err, errInjected) {
		t.Errorf("lookup failure: %v", err)
	}
	delete(f.store.fail, "LookupSubjectPassword")
	if _, ok, err := verifySubjectPassword(ctx, f.store, uuid.MustNew(), mfaTestPassword); ok || err != nil {
		t.Errorf("unknown account: ok=%v err=%v", ok, err)
	}
	login, ok, err := verifySubjectPassword(ctx, f.store, f.subject, mfaTestPassword)
	if !ok || err != nil || login != "ada@example.com" {
		t.Errorf("right password: login=%q ok=%v err=%v", login, ok, err)
	}
	if _, ok, err := verifySubjectPassword(ctx, f.store, f.subject, "wrong password"); ok || err != nil {
		t.Errorf("wrong password: ok=%v err=%v", ok, err)
	}
}
