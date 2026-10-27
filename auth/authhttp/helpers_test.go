package authhttp

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

type loginStoreStub struct {
	identity   auth.PasswordIdentity
	found      bool
	lookupErr  error
	replaceErr error
	replaced   bool
	lookEmail  string
	lookTenant string
}

func (s *loginStoreStub) LookupPassword(_ context.Context, email, tenant string) (auth.PasswordIdentity, bool, error) {
	s.lookEmail, s.lookTenant = email, tenant
	return s.identity, s.found, s.lookupErr
}

func (s *loginStoreStub) ReplacePasswordHash(_ context.Context, subjectID uuid.UUID, previous, next string) (bool, error) {
	if subjectID != s.identity.SubjectID || previous != s.identity.PasswordHash || next == "" || next == previous {
		return false, errors.New("unexpected password replacement")
	}
	return s.replaced, s.replaceErr
}

type sessionStub struct {
	membership        uuid.UUID
	credentialVersion int64
	expiresAt         time.Time
	token             auth.Token
	err               error
}

func (s *sessionStub) CreateSession(_ context.Context, membership uuid.UUID, expiresAt time.Time) (auth.Token, error) {
	s.membership, s.expiresAt = membership, expiresAt
	return s.token, s.err
}

func (s *sessionStub) CreateSessionAtVersion(_ context.Context, membership uuid.UUID, credentialVersion int64, expiresAt time.Time) (auth.Token, error) {
	s.membership, s.credentialVersion, s.expiresAt = membership, credentialVersion, expiresAt
	return s.token, s.err
}

type securityAuditorStub struct {
	events []auth.SecurityEvent
	err    error
}

func (s *securityAuditorStub) RecordSecurityEvent(_ context.Context, event auth.SecurityEvent) error {
	s.events = append(s.events, event)
	return s.err
}

type revokingSessionStub struct {
	sessionStub
	revoked   auth.Token
	revokeErr error
}

func (s *revokingSessionStub) RevokeSession(_ context.Context, token auth.Token) error {
	s.revoked = token
	return s.revokeErr
}

type mfaStoreStub struct {
	pending                  auth.TOTPSecretRecord
	pendingFound             bool
	confirmResult            bool
	confirmCounter           int64
	backupDigests            []auth.Digest
	challenge                auth.MFAChallengeRecord
	challengeFound           bool
	consumeResult            bool
	consumeMembership        uuid.UUID
	consumeCounter           int64
	createdChallenge         auth.Digest
	createdMembership        uuid.UUID
	createdCredentialVersion int64
	createdExpiresAt         time.Time
	createdSessionExpiry     time.Time
	rotationStarted          bool
	rotationConfirmResult    bool
	disabledResult           bool
	err                      error
}

func (s *mfaStoreStub) BeginTOTPEnrollment(_ context.Context, subjectID uuid.UUID, digest auth.Digest, ciphertext []byte, expiresAt time.Time) error {
	if s.err != nil {
		return s.err
	}
	s.pending = auth.TOTPSecretRecord{
		SubjectID:        subjectID,
		SecretDigest:     digest,
		SecretCiphertext: append([]byte(nil), ciphertext...),
		ExpiresAt:        expiresAt,
	}
	s.pendingFound = true
	return nil
}

func (s *mfaStoreStub) BeginTOTPRotation(_ context.Context, subjectID uuid.UUID, digest auth.Digest, ciphertext []byte, expiresAt time.Time, _ *auth.Digest) error {
	if err := s.BeginTOTPEnrollment(context.Background(), subjectID, digest, ciphertext, expiresAt); err != nil {
		return err
	}
	// A rotation's pending key sits beside an active factor.
	s.pending.FactorActive = true
	s.rotationStarted = true
	return nil
}

func (s *mfaStoreStub) ConfirmTOTPRotation(_ context.Context, _ uuid.UUID, _ auth.Digest, counter int64, backupCodeDigests []auth.Digest, _ *auth.Digest) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	s.confirmCounter = counter
	s.backupDigests = append([]auth.Digest(nil), backupCodeDigests...)
	return s.rotationConfirmResult, nil
}

func (s *mfaStoreStub) DisableMFA(context.Context, uuid.UUID, *auth.Digest) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.disabledResult, nil
}

func (s *mfaStoreStub) LoadPendingTOTP(context.Context, uuid.UUID) (auth.TOTPSecretRecord, bool, error) {
	if s.err != nil {
		return auth.TOTPSecretRecord{}, false, s.err
	}
	return s.pending, s.pendingFound, nil
}

func (s *mfaStoreStub) ConfirmTOTPEnrollment(_ context.Context, _ uuid.UUID, _ auth.Digest, counter int64, backupCodeDigests []auth.Digest) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	s.confirmCounter = counter
	s.backupDigests = append([]auth.Digest(nil), backupCodeDigests...)
	return s.confirmResult, nil
}

func (s *mfaStoreStub) CreateMFAChallenge(_ context.Context, digest auth.Digest, membershipID uuid.UUID, credentialVersion int64, expiresAt, sessionExpiresAt time.Time) error {
	if s.err != nil {
		return s.err
	}
	s.createdChallenge = digest
	s.createdMembership = membershipID
	s.createdCredentialVersion = credentialVersion
	s.createdExpiresAt = expiresAt
	s.createdSessionExpiry = sessionExpiresAt
	return nil
}

func (s *mfaStoreStub) LoadMFAChallenge(context.Context, auth.Digest) (auth.MFAChallengeRecord, bool, error) {
	if s.err != nil {
		return auth.MFAChallengeRecord{}, false, s.err
	}
	return s.challenge, s.challengeFound, nil
}

func (s *mfaStoreStub) ConsumeMFAChallenge(_ context.Context, _ auth.Digest, _ auth.Digest, counter int64) (uuid.UUID, bool, error) {
	if s.err != nil {
		return uuid.UUID{}, false, s.err
	}
	s.consumeCounter = counter
	if !s.consumeResult {
		return uuid.UUID{}, false, nil
	}
	return s.consumeMembership, true, nil
}

func (s *mfaStoreStub) ConsumeMFABackupChallenge(_ context.Context, _ auth.Digest, _ auth.Digest) (uuid.UUID, bool, error) {
	if s.err != nil {
		return uuid.UUID{}, false, s.err
	}
	if !s.consumeResult {
		return uuid.UUID{}, false, nil
	}
	return s.consumeMembership, true, nil
}

type mfaIssuerStub struct {
	token             auth.MFAChallengeToken
	expiresAt         time.Time
	membershipID      uuid.UUID
	credentialVersion int64
	sessionExpiresAt  time.Time
	err               error
}

func (s *mfaIssuerStub) IssueMFAChallenge(_ context.Context, membershipID uuid.UUID, credentialVersion int64, sessionExpiresAt time.Time) (auth.MFAChallengeToken, time.Time, error) {
	s.membershipID = membershipID
	s.credentialVersion = credentialVersion
	s.sessionExpiresAt = sessionExpiresAt
	return s.token, s.expiresAt, s.err
}

type recoveryStoreStub struct {
	identity       auth.RecoveryIdentity
	found          bool
	lookupErr      error
	createErr      error
	invalidateErr  error
	consumeErr     error
	consumeResult  bool
	lookupEmail    string
	lookupTenant   string
	createdSubject uuid.UUID
	createdDigest  auth.Digest
	createdExpiry  time.Time
	invalidated    auth.Digest
	consumedDigest auth.Digest
	consumedHash   string
}

func (s *recoveryStoreStub) LookupRecovery(_ context.Context, email, tenant string) (auth.RecoveryIdentity, bool, error) {
	s.lookupEmail, s.lookupTenant = email, tenant
	return s.identity, s.found, s.lookupErr
}

func (s *recoveryStoreStub) CreateRecovery(_ context.Context, subjectID uuid.UUID, digest auth.Digest, expiresAt time.Time) error {
	s.createdSubject, s.createdDigest, s.createdExpiry = subjectID, digest, expiresAt
	return s.createErr
}

func (s *recoveryStoreStub) InvalidateRecovery(_ context.Context, digest auth.Digest) error {
	s.invalidated = digest
	return s.invalidateErr
}

func (s *recoveryStoreStub) ConsumeRecovery(_ context.Context, digest auth.Digest, hash string) (bool, error) {
	s.consumedDigest, s.consumedHash = digest, hash
	return s.consumeResult, s.consumeErr
}

type recoverySenderStub struct {
	message auth.PasswordRecoveryMessage
	err     error
	calls   int
}

func (s *recoverySenderStub) SendPasswordRecovery(_ context.Context, message auth.PasswordRecoveryMessage) error {
	s.message = message
	s.calls++
	return s.err
}

var errInjected = errors.New("injected store failure")

// fakeArgon2idKDF stands in for golang.org/x/crypto/argon2.IDKey, which the
// core does not link (ADR 0053): a fast, deterministic function of every
// input with IDKey's signature, registered for this package's tests so the
// default hasher writes and verifies $argon2id$ hashes as in an application
// that links an identity adapter. The real Argon2id is tested by the
// adapters.
func fakeArgon2idKDF(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
	digest := sha256.New()
	var header [17]byte
	binary.BigEndian.PutUint32(header[0:], time)
	binary.BigEndian.PutUint32(header[4:], memory)
	header[8] = threads
	binary.BigEndian.PutUint32(header[9:], keyLen)
	binary.BigEndian.PutUint32(header[13:], uint32(len(salt))) // #nosec G115 -- salts are at most 64 bytes
	digest.Write(header[:])
	digest.Write(salt)
	digest.Write(password)
	seed := digest.Sum(nil)
	key := make([]byte, 0, keyLen+sha256.Size)
	for block := byte(0); uint32(len(key)) < keyLen; block++ { // #nosec G115 -- keys are 32 bytes
		sum := sha256.Sum256(append([]byte{block}, seed...))
		key = append(key, sum[:]...)
	}
	return key[:keyLen]
}

func init() {
	auth.RegisterArgon2id(fakeArgon2idKDF)
}

// The fixtures share one password hash.
var mfaTestPasswordHash = sync.OnceValues(func() (string, error) { return auth.HashPassword(mfaTestPassword) })

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
	return &loginStoreStub{found: true, identity: auth.PasswordIdentity{
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

	challenge      auth.MFAChallengeRecord
	challengeFound bool
	membership     uuid.UUID
}

func (s *faultMFAStore) failed(method string) error {
	if hook := s.before[method]; hook != nil {
		hook()
	}
	return s.fail[method]
}

func (s *faultMFAStore) LookupSubjectPassword(ctx context.Context, subject uuid.UUID) (auth.SubjectPassword, bool, error) {
	if err := s.failed("LookupSubjectPassword"); err != nil {
		return auth.SubjectPassword{}, false, err
	}
	return s.memoryMFAAccounts.LookupSubjectPassword(ctx, subject)
}

func (s *faultMFAStore) BeginTOTPEnrollment(ctx context.Context, subject uuid.UUID, digest auth.Digest, ciphertext []byte, expiresAt time.Time) error {
	if err := s.failed("BeginTOTPEnrollment"); err != nil {
		return err
	}
	return s.memoryMFAAccounts.BeginTOTPEnrollment(ctx, subject, digest, ciphertext, expiresAt)
}

func (s *faultMFAStore) BeginTOTPRotation(ctx context.Context, subject uuid.UUID, digest auth.Digest, ciphertext []byte, expiresAt time.Time, stepUp *auth.Digest) error {
	if err := s.failed("BeginTOTPRotation"); err != nil {
		return err
	}
	return s.memoryMFAAccounts.BeginTOTPRotation(ctx, subject, digest, ciphertext, expiresAt, stepUp)
}

func (s *faultMFAStore) LoadPendingTOTP(ctx context.Context, subject uuid.UUID) (auth.TOTPSecretRecord, bool, error) {
	if err := s.failed("LoadPendingTOTP"); err != nil {
		return auth.TOTPSecretRecord{}, false, err
	}
	return s.memoryMFAAccounts.LoadPendingTOTP(ctx, subject)
}

func (s *faultMFAStore) ConfirmTOTPEnrollment(ctx context.Context, subject uuid.UUID, digest auth.Digest, counter int64, backup []auth.Digest) (bool, error) {
	if err := s.failed("ConfirmTOTPEnrollment"); err != nil {
		return false, err
	}
	if s.deny["ConfirmTOTPEnrollment"] {
		return false, nil
	}
	return s.memoryMFAAccounts.ConfirmTOTPEnrollment(ctx, subject, digest, counter, backup)
}

func (s *faultMFAStore) ConfirmTOTPRotation(ctx context.Context, subject uuid.UUID, digest auth.Digest, counter int64, backup []auth.Digest, stepUp *auth.Digest) (bool, error) {
	if err := s.failed("ConfirmTOTPRotation"); err != nil {
		return false, err
	}
	if s.deny["ConfirmTOTPRotation"] {
		return false, nil
	}
	return s.memoryMFAAccounts.ConfirmTOTPRotation(ctx, subject, digest, counter, backup, stepUp)
}

func (s *faultMFAStore) DisableMFA(ctx context.Context, subject uuid.UUID, stepUp *auth.Digest) (bool, error) {
	if err := s.failed("DisableMFA"); err != nil {
		return false, err
	}
	return s.memoryMFAAccounts.DisableMFA(ctx, subject, stepUp)
}

func (s *faultMFAStore) LoadMFAStatus(ctx context.Context, subject uuid.UUID) (auth.MFAStatus, error) {
	if err := s.failed("LoadMFAStatus"); err != nil {
		return auth.MFAStatus{}, err
	}
	return s.memoryMFAAccounts.LoadMFAStatus(ctx, subject)
}

func (s *faultMFAStore) LoadActiveTOTP(ctx context.Context, subject uuid.UUID) (auth.TOTPSecretRecord, bool, error) {
	if err := s.failed("LoadActiveTOTP"); err != nil {
		return auth.TOTPSecretRecord{}, false, err
	}
	return s.memoryMFAAccounts.LoadActiveTOTP(ctx, subject)
}

func (s *faultMFAStore) AdvanceTOTPCounter(ctx context.Context, subject uuid.UUID, digest auth.Digest, counter int64) (bool, error) {
	if err := s.failed("AdvanceTOTPCounter"); err != nil {
		return false, err
	}
	if s.deny["AdvanceTOTPCounter"] {
		return false, nil
	}
	return s.memoryMFAAccounts.AdvanceTOTPCounter(ctx, subject, digest, counter)
}

func (s *faultMFAStore) ConsumeMFABackupCode(ctx context.Context, subject uuid.UUID, digest auth.Digest) (bool, error) {
	if err := s.failed("ConsumeMFABackupCode"); err != nil {
		return false, err
	}
	return s.memoryMFAAccounts.ConsumeMFABackupCode(ctx, subject, digest)
}

func (s *faultMFAStore) HasMFABackupCode(ctx context.Context, subject uuid.UUID, digest auth.Digest) (bool, error) {
	if err := s.failed("HasMFABackupCode"); err != nil {
		return false, err
	}
	return s.memoryMFAAccounts.HasMFABackupCode(ctx, subject, digest)
}

func (s *faultMFAStore) ReplaceMFABackupCodes(ctx context.Context, subject uuid.UUID, digests []auth.Digest, stepUp *auth.Digest) (bool, error) {
	if err := s.failed("ReplaceMFABackupCodes"); err != nil {
		return false, err
	}
	return s.memoryMFAAccounts.ReplaceMFABackupCodes(ctx, subject, digests, stepUp)
}

func (s *faultMFAStore) CreateMFAChallenge(context.Context, auth.Digest, uuid.UUID, int64, time.Time, time.Time) error {
	return s.failed("CreateMFAChallenge")
}

func (s *faultMFAStore) LoadMFAChallenge(context.Context, auth.Digest) (auth.MFAChallengeRecord, bool, error) {
	if err := s.failed("LoadMFAChallenge"); err != nil {
		return auth.MFAChallengeRecord{}, false, err
	}
	return s.challenge, s.challengeFound, nil
}

func (s *faultMFAStore) ConsumeMFAChallenge(context.Context, auth.Digest, auth.Digest, int64) (uuid.UUID, bool, error) {
	if err := s.failed("ConsumeMFAChallenge"); err != nil {
		return uuid.UUID{}, false, err
	}
	return s.membership, !s.deny["ConsumeMFAChallenge"], nil
}

func (s *faultMFAStore) ConsumeMFABackupChallenge(context.Context, auth.Digest, auth.Digest) (uuid.UUID, bool, error) {
	if err := s.failed("ConsumeMFABackupChallenge"); err != nil {
		return uuid.UUID{}, false, err
	}
	return s.membership, !s.deny["ConsumeMFABackupChallenge"], nil
}

// faultMFA is one service over a faultMFAStore with an enrolled account.
type faultMFA struct {
	t       *testing.T
	store   *faultMFAStore
	service *auth.MFAService
	subject uuid.UUID
	now     time.Time
	secret  []byte
	audits  []auth.SecurityEvent
	// auditErr, when set, is returned by the auditor after it records.
	auditErr error
	sessions *sessionStub
	// cipher seals the account's secrets, as the service's does.
	cipher *auth.AESGCMSecretCipher
}

func newFaultMFA(t *testing.T, options ...auth.MFAOption) *faultMFA {
	t.Helper()
	f := &faultMFA{
		t:       t,
		subject: uuid.MustNew(),
		now:     time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
		secret:  []byte("12345678901234567890"),
	}
	hash := cachedPasswordHash(t)
	cipher, err := auth.NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	f.cipher = cipher
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	f.sessions = &sessionStub{token: token}
	memory := &memoryMFAAccounts{accounts: map[uuid.UUID]*mfaAccount{
		f.subject: {login: "ada@example.com", hash: hash},
	}}
	f.store = &faultMFAStore{memoryMFAAccounts: memory, fail: map[string]error{}, deny: map[string]bool{}, before: map[string]func(){}, membership: uuid.MustNew()}
	auditor := auth.SecurityAuditorFunc(func(_ context.Context, event auth.SecurityEvent) error {
		f.audits = append(f.audits, event)
		return f.auditErr
	})
	options = append([]auth.MFAOption{auth.WithMFAClock(func() time.Time { return f.now }), auth.WithMFAAuditor(auditor)}, options...)
	f.service, err = auth.NewMFAService(f.store, f.sessions, cipher, options...)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// enroll gives the account an active factor over f.secret and returns its
// sealed secret record.
func (f *faultMFA) enroll() {
	f.t.Helper()
	cipher, err := auth.NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
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
	backup := map[auth.Digest]bool{}
	for _, digest := range digests {
		backup[digest] = false
	}
	account := f.store.accounts[f.subject]
	account.factor = &auth.TOTPSecretRecord{SubjectID: f.subject, SecretDigest: digestOf(f.secret), SecretCiphertext: sealed, LastCounter: -1}
	account.backupCodes = backup
}

func (f *faultMFA) code() string { return totpCode(f.secret, uint64(f.now.Unix()/totpPeriodSeconds)) }

func digestOf(secret []byte) auth.Digest { return sha256.Sum256(secret) }

// failingThrottler fails or denies the MFA throttle.
type failingThrottler struct {
	attemptErr, successErr error
	allowed                bool
}

func (t failingThrottler) Attempt(context.Context, auth.LoginThrottleKey) (auth.LoginThrottleDecision, error) {
	return auth.LoginThrottleDecision{Allowed: t.allowed, RetryAfter: 7 * time.Second}, t.attemptErr
}

func (t failingThrottler) Success(context.Context, auth.LoginThrottleKey) error { return t.successErr }

// holdSlot takes one slot of limiter until the returned release is called.
func holdSlot(t *testing.T, limiter *auth.PasswordWorkLimiter) (release func()) {
	t.Helper()
	held, done := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- limiter.Do(context.Background(), func() {
			close(held)
			<-done
		})
	}()
	select {
	case <-held:
	case err := <-finished:
		t.Fatalf("could not take a slot: %v", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			if err := <-finished; err != nil {
				t.Errorf("held slot: %v", err)
			}
		})
	}
}

func knownRecoveryStore() *recoveryStoreStub {
	return &recoveryStoreStub{
		found:    true,
		identity: auth.RecoveryIdentity{SubjectID: uuid.MustNew(), Email: "alice@example.com"},
	}
}

func newRehashStore(t *testing.T, hash string) *rehashStore {
	t.Helper()
	return &rehashStore{identity: auth.PasswordIdentity{
		SubjectID: uuid.MustNew(), MembershipID: uuid.MustNew(), PasswordHash: hash, CredentialVersion: 1,
	}}
}

func mustRecoveryToken(t *testing.T) auth.RecoveryToken {
	t.Helper()
	token, err := auth.NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// cheapPasswordHash hashes with the lowest work factor auth accepts, a
// $pbkdf2-sha256$ hash at 100,000 iterations written as auth writes it. Tests
// that verify a password many times, MFA step-ups above all, use it so the
// package stays inside the test timeout under -race.
func cheapPasswordHash(password string) (string, error) {
	const iterations = 100_000
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$pbkdf2-sha256$v=1$i=%d$%s$%s", iterations,
		base64.RawURLEncoding.EncodeToString(salt), base64.RawURLEncoding.EncodeToString(key)), nil
}

// cheapPasswordHasher is NewPasswordHasher(options...) writing Argon2id
// (the registered fake) without the legacy timing floor unless options turn
// it on.
func cheapPasswordHasher(t testing.TB, options ...auth.PasswordHasherOption) *auth.PasswordHasher {
	t.Helper()
	defaults := []auth.PasswordHasherOption{auth.WithPasswordAlgorithm(auth.PasswordAlgorithmArgon2id), auth.WithLegacyHashTimingFloor(false)}
	hasher, err := auth.NewPasswordHasher(append(defaults, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	return hasher
}

// rehashStore is a PasswordStore holding one account whose hash
// ReplacePasswordHash really replaces.
type rehashStore struct {
	mu       sync.Mutex
	identity auth.PasswordIdentity
	replaced []string
}

func (s *rehashStore) LookupPassword(_ context.Context, email, tenant string) (auth.PasswordIdentity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identity, email == "ada@example.com" && tenant == "acme", nil
}

func (s *rehashStore) ReplacePasswordHash(_ context.Context, subjectID uuid.UUID, previous, next string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if subjectID != s.identity.SubjectID || previous != s.identity.PasswordHash {
		return false, nil
	}
	s.identity.PasswordHash = next
	s.replaced = append(s.replaced, next)
	return true, nil
}
