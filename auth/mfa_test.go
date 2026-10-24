package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

type mfaStoreStub struct {
	pending                  TOTPSecretRecord
	pendingFound             bool
	confirmResult            bool
	confirmCounter           int64
	backupDigests            []Digest
	challenge                MFAChallengeRecord
	challengeFound           bool
	consumeResult            bool
	consumeMembership        uuid.UUID
	consumeCounter           int64
	createdChallenge         Digest
	createdMembership        uuid.UUID
	createdCredentialVersion int64
	createdExpiresAt         time.Time
	createdSessionExpiry     time.Time
	rotationStarted          bool
	rotationConfirmResult    bool
	disabledResult           bool
	err                      error
}

func (s *mfaStoreStub) BeginTOTPEnrollment(_ context.Context, subjectID uuid.UUID, digest Digest, ciphertext []byte, expiresAt time.Time) error {
	if s.err != nil {
		return s.err
	}
	s.pending = TOTPSecretRecord{
		SubjectID:        subjectID,
		SecretDigest:     digest,
		SecretCiphertext: append([]byte(nil), ciphertext...),
		ExpiresAt:        expiresAt,
	}
	s.pendingFound = true
	return nil
}

func (s *mfaStoreStub) BeginTOTPRotation(_ context.Context, subjectID uuid.UUID, digest Digest, ciphertext []byte, expiresAt time.Time, _ *Digest) error {
	if err := s.BeginTOTPEnrollment(context.Background(), subjectID, digest, ciphertext, expiresAt); err != nil {
		return err
	}
	// A rotation's pending key sits beside an active factor.
	s.pending.FactorActive = true
	s.rotationStarted = true
	return nil
}

func (s *mfaStoreStub) ConfirmTOTPRotation(_ context.Context, _ uuid.UUID, _ Digest, counter int64, backupCodeDigests []Digest, _ *Digest) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	s.confirmCounter = counter
	s.backupDigests = append([]Digest(nil), backupCodeDigests...)
	return s.rotationConfirmResult, nil
}

func (s *mfaStoreStub) DisableMFA(context.Context, uuid.UUID, *Digest) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.disabledResult, nil
}

func (s *mfaStoreStub) LoadPendingTOTP(context.Context, uuid.UUID) (TOTPSecretRecord, bool, error) {
	if s.err != nil {
		return TOTPSecretRecord{}, false, s.err
	}
	return s.pending, s.pendingFound, nil
}

func (s *mfaStoreStub) ConfirmTOTPEnrollment(_ context.Context, _ uuid.UUID, _ Digest, counter int64, backupCodeDigests []Digest) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	s.confirmCounter = counter
	s.backupDigests = append([]Digest(nil), backupCodeDigests...)
	return s.confirmResult, nil
}

func (s *mfaStoreStub) CreateMFAChallenge(_ context.Context, digest Digest, membershipID uuid.UUID, credentialVersion int64, expiresAt, sessionExpiresAt time.Time) error {
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

func (s *mfaStoreStub) LoadMFAChallenge(context.Context, Digest) (MFAChallengeRecord, bool, error) {
	if s.err != nil {
		return MFAChallengeRecord{}, false, s.err
	}
	return s.challenge, s.challengeFound, nil
}

func (s *mfaStoreStub) ConsumeMFAChallenge(_ context.Context, _ Digest, _ Digest, counter int64) (uuid.UUID, bool, error) {
	if s.err != nil {
		return uuid.UUID{}, false, s.err
	}
	s.consumeCounter = counter
	if !s.consumeResult {
		return uuid.UUID{}, false, nil
	}
	return s.consumeMembership, true, nil
}

func (s *mfaStoreStub) ConsumeMFABackupChallenge(_ context.Context, _ Digest, _ Digest) (uuid.UUID, bool, error) {
	if s.err != nil {
		return uuid.UUID{}, false, s.err
	}
	if !s.consumeResult {
		return uuid.UUID{}, false, nil
	}
	return s.consumeMembership, true, nil
}

type mfaIssuerStub struct {
	token             MFAChallengeToken
	expiresAt         time.Time
	membershipID      uuid.UUID
	credentialVersion int64
	sessionExpiresAt  time.Time
	err               error
}

func (s *mfaIssuerStub) IssueMFAChallenge(_ context.Context, membershipID uuid.UUID, credentialVersion int64, sessionExpiresAt time.Time) (MFAChallengeToken, time.Time, error) {
	s.membershipID = membershipID
	s.credentialVersion = credentialVersion
	s.sessionExpiresAt = sessionExpiresAt
	return s.token, s.expiresAt, s.err
}

func TestAESGCMSecretCipherBase64RequiresCanonical256BitKey(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	cipher, err := NewAESGCMSecretCipherBase64(encoded)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := cipher.Seal([]byte("seed material"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := cipher.Open(sealed)
	if err != nil || string(opened) != "seed material" {
		t.Fatalf("opened=%q err=%v", opened, err)
	}

	for _, invalid := range []string{
		"",
		encoded + "=",
		base64.RawURLEncoding.EncodeToString(raw[:31]),
		"not*base64url",
	} {
		if _, err := NewAESGCMSecretCipherBase64(invalid); err == nil {
			t.Fatalf("accepted invalid MFA key %q", invalid)
		}
	}
}

func TestDefaultMFAThrottleConfigIsValid(t *testing.T) {
	config := DefaultMFAThrottleConfig()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	if config.Window != 5*time.Minute || config.AccountLimit != 5 || config.SourceLimit != 60 || config.MaxEntries != 20_000 {
		t.Fatalf("config=%+v", config)
	}
}

func TestAESGCMSecretCipherRoundTripAndTamperDetection(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	cipher, err := NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("01234567890123456789")
	sealed, err := cipher.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if string(sealed) == string(plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}
	opened, err := cipher.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != string(plaintext) {
		t.Fatalf("opened=%q", opened)
	}
	sealed[len(sealed)-1] ^= 0x01
	if _, err := cipher.Open(sealed); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestTOTPUsesRFC4226HOTPValues(t *testing.T) {
	secret := []byte("12345678901234567890")
	want := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for counter, expected := range want {
		if got := totpCode(secret, uint64(counter)); got != expected {
			t.Fatalf("counter=%d got=%s want=%s", counter, got, expected)
		}
	}
}

func TestTOTPEnrollmentRequiresConfirmationAndPreventsCodeReplay(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store := &mfaStoreStub{confirmResult: true}
	sessions := &sessionStub{}
	now := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	service, err := NewMFAService(store, sessions, secretCipher, WithMFAClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	subjectID := uuid.MustNew()
	enrollment, err := service.BeginTOTPEnrollment(context.Background(), subjectID, "Forge", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if enrollment.Secret.Reveal() == "" || !strings.HasPrefix(enrollment.ProvisioningURI.Reveal(), "otpauth://totp/") {
		t.Fatalf("enrollment secret=%s uri=%s", enrollment.Secret, enrollment.ProvisioningURI)
	}
	if enrollment.Secret.String() != "[REDACTED]" || enrollment.ProvisioningURI.String() != "[REDACTED]" {
		t.Fatal("TOTP enrollment secrets do not redact")
	}
	rawSecret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	code := totpCode(rawSecret, uint64(now.Unix()/totpPeriodSeconds))
	confirmation, err := service.ConfirmTOTPEnrollment(context.Background(), subjectID, code)
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmation.BackupCodes) != DefaultMFABackupCodeCount || len(store.backupDigests) != DefaultMFABackupCodeCount {
		t.Fatalf("backup codes=%d digests=%d", len(confirmation.BackupCodes), len(store.backupDigests))
	}
	for _, backupCode := range confirmation.BackupCodes {
		if backupCode.Reveal() == "" || backupCode.String() != "[REDACTED]" {
			t.Fatalf("backup code=%s reveal=%q", backupCode, backupCode.Reveal())
		}
	}
	counter := now.Unix() / totpPeriodSeconds
	if store.confirmCounter != counter {
		t.Fatalf("confirmed counter=%d want=%d", store.confirmCounter, counter)
	}
	if _, ok := matchTOTPCounter(rawSecret, code, now, counter); ok {
		t.Fatal("same TOTP counter was accepted twice")
	}
}

func TestLoginWithMFAIssuesChallengeInsteadOfSession(t *testing.T) {
	hash, err := HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	membershipID := uuid.MustNew()
	store := &loginStoreStub{
		found: true,
		identity: PasswordIdentity{
			SubjectID:         uuid.MustNew(),
			MembershipID:      membershipID,
			PasswordHash:      hash,
			CredentialVersion: 1,
			MFARequired:       true,
		},
	}
	sessions := &sessionStub{}
	now := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	issuer := &mfaIssuerStub{token: challenge, expiresAt: now.Add(5 * time.Minute)}
	service, err := NewLoginService(
		store,
		sessions,
		WithLoginClock(func() time.Time { return now }),
		WithMFAChallengeIssuer(issuer),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Login(context.Background(), LoginInput{
		Email: "alice@example.com", Password: "correct-password", TenantSlug: "acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.MFARequired || result.MFAChallenge.Digest() != challenge.Digest() {
		t.Fatalf("result=%+v", result)
	}
	if sessions.membership != (uuid.UUID{}) {
		t.Fatal("password login created a session before MFA")
	}
	if issuer.membershipID != membershipID || issuer.credentialVersion != 1 || !issuer.sessionExpiresAt.Equal(now.Add(defaultSessionTTL)) {
		t.Fatalf("issuer membership=%s version=%d session expiry=%v", issuer.membershipID, issuer.credentialVersion, issuer.sessionExpiresAt)
	}
}

func TestMFACompletionConsumesChallengeBeforeCreatingSession(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("12345678901234567890")
	sealed, err := secretCipher.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(secret)
	challengeToken, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	sessionToken, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	membershipID := uuid.MustNew()
	store := &mfaStoreStub{
		challengeFound: true,
		challenge: MFAChallengeRecord{
			SubjectID:         uuid.MustNew(),
			MembershipID:      membershipID,
			CredentialVersion: 1,
			SecretDigest:      digest,
			SecretCiphertext:  sealed,
			LastCounter:       -1,
			ExpiresAt:         now.Add(5 * time.Minute),
			SessionExpiresAt:  now.Add(12 * time.Hour),
		},
		consumeResult:     true,
		consumeMembership: membershipID,
	}
	sessions := &sessionStub{token: sessionToken}
	auditor := &securityAuditorStub{}
	service, err := NewMFAService(
		store,
		sessions,
		secretCipher,
		WithMFAClock(func() time.Time { return now }),
		WithMFAAuditor(auditor),
	)
	if err != nil {
		t.Fatal(err)
	}
	code := totpCode(secret, uint64(now.Unix()/totpPeriodSeconds))
	result, err := service.CompleteLogin(context.Background(), MFACompletion{
		ChallengeToken: challengeToken.Reveal(),
		Code:           code,
		Source:         "192.0.2.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.consumeCounter != now.Unix()/totpPeriodSeconds {
		t.Fatalf("consume counter=%d", store.consumeCounter)
	}
	if sessions.membership != membershipID || sessions.credentialVersion != 1 || !sessions.expiresAt.Equal(now.Add(12*time.Hour)) {
		t.Fatalf("session membership=%s version=%d expires=%v", sessions.membership, sessions.credentialVersion, sessions.expiresAt)
	}
	if result.Token.Digest() != sessionToken.Digest() || !result.ExpiresAt.Equal(now.Add(12*time.Hour)) {
		t.Fatalf("result=%+v", result)
	}
	if len(auditor.events) != 1 ||
		auditor.events[0].Kind != SecurityLoginSucceeded ||
		auditor.events[0].Outcome != SecurityOutcomeSucceeded ||
		auditor.events[0].MembershipID != membershipID ||
		auditor.events[0].CredentialDigest != sessionToken.Digest() ||
		auditor.events[0].SourceDigest == (Digest{}) {
		t.Fatalf("events=%+v", auditor.events)
	}
}

func TestMFACompletionFailsClosedOnConsumeRace(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("12345678901234567890")
	sealed, err := secretCipher.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(secret)
	challengeToken, _ := NewMFAChallengeToken()
	now := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	store := &mfaStoreStub{
		challengeFound: true,
		challenge: MFAChallengeRecord{
			SubjectID:         uuid.MustNew(),
			MembershipID:      uuid.MustNew(),
			CredentialVersion: 1,
			SecretDigest:      digest,
			SecretCiphertext:  sealed,
			LastCounter:       -1,
			ExpiresAt:         now.Add(5 * time.Minute),
			SessionExpiresAt:  now.Add(time.Hour),
		},
		consumeResult: false,
	}
	sessions := &sessionStub{}
	auditor := &securityAuditorStub{}
	service, err := NewMFAService(
		store,
		sessions,
		secretCipher,
		WithMFAClock(func() time.Time { return now }),
		WithMFAAuditor(auditor),
	)
	if err != nil {
		t.Fatal(err)
	}
	code := totpCode(secret, uint64(now.Unix()/totpPeriodSeconds))
	_, err = service.CompleteLogin(context.Background(), MFACompletion{ChallengeToken: challengeToken.Reveal(), Code: code})
	if !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("error=%v", err)
	}
	if sessions.membership != (uuid.UUID{}) {
		t.Fatal("session created after MFA consume race")
	}
	if len(auditor.events) != 1 ||
		auditor.events[0].Kind != SecurityMFAChallengeFailed ||
		auditor.events[0].Outcome != SecurityOutcomeDenied ||
		auditor.events[0].CredentialDigest != challengeToken.Digest() {
		t.Fatalf("events=%+v", auditor.events)
	}
}

func FuzzParseMFAChallengeTokenNeverPanics(f *testing.F) {
	for _, seed := range []string{"", "x", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, secret string) {
		_, _ = ParseMFAChallengeToken(secret)
	})
}

func TestMFARotationAndDisableLifecycle(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store := &mfaStoreStub{rotationConfirmResult: true, disabledResult: true}
	service, err := NewMFAService(store, &sessionStub{}, secretCipher)
	if err != nil {
		t.Fatal(err)
	}
	subjectID := uuid.MustNew()
	enrollment, err := service.BeginTOTPRotation(context.Background(), subjectID, "Forge", "alice@example.com", changeAuthorizedBy(subjectID))
	if err != nil {
		t.Fatal(err)
	}
	if !store.rotationStarted {
		t.Fatal("rotation store was not used")
	}
	rawSecret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	now := service.now().UTC()
	code := totpCode(rawSecret, uint64(now.Unix()/totpPeriodSeconds))
	confirmation, err := service.ConfirmTOTPRotation(context.Background(), subjectID, code, changeAuthorizedBy(subjectID))
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmation.BackupCodes) != DefaultMFABackupCodeCount || len(store.backupDigests) != DefaultMFABackupCodeCount {
		t.Fatalf("backup codes=%d digests=%d", len(confirmation.BackupCodes), len(store.backupDigests))
	}
	if err := service.DisableMFA(context.Background(), subjectID, changeAuthorizedBy(subjectID)); err != nil {
		t.Fatal(err)
	}
}

func TestMFADisableFailsWhenNotEnabled(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewMFAService(&mfaStoreStub{}, &sessionStub{}, secretCipher)
	if err != nil {
		t.Fatal(err)
	}
	subjectID := uuid.MustNew()
	if err := service.DisableMFA(context.Background(), subjectID, changeAuthorizedBy(subjectID)); !errors.Is(err, ErrMFANotEnabled) {
		t.Fatalf("error=%v", err)
	}
}

func TestMFACompletionRevokesSessionWhenSuccessAuditFails(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("12345678901234567890")
	sealed, err := secretCipher.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	sessionToken, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 21, 30, 0, 0, time.UTC)
	membershipID := uuid.MustNew()
	store := &mfaStoreStub{
		challengeFound: true,
		challenge: MFAChallengeRecord{
			SubjectID:         uuid.MustNew(),
			MembershipID:      membershipID,
			CredentialVersion: 4,
			SecretDigest:      sha256.Sum256(secret),
			SecretCiphertext:  sealed,
			LastCounter:       -1,
			ExpiresAt:         now.Add(5 * time.Minute),
			SessionExpiresAt:  now.Add(time.Hour),
		},
		consumeResult:     true,
		consumeMembership: membershipID,
	}
	sessions := &revokingSessionStub{sessionStub: sessionStub{token: sessionToken}}
	auditCause := errors.New("audit unavailable")
	service, err := NewMFAService(
		store,
		sessions,
		secretCipher,
		WithMFAClock(func() time.Time { return now }),
		WithMFAAuditor(&securityAuditorStub{err: auditCause}),
	)
	if err != nil {
		t.Fatal(err)
	}
	code := totpCode(secret, uint64(now.Unix()/totpPeriodSeconds))
	result, err := service.CompleteLogin(context.Background(), MFACompletion{
		ChallengeToken: challenge.Reveal(),
		Code:           code,
		Source:         "192.0.2.10",
	})
	if !errors.Is(err, auditCause) {
		t.Fatalf("error=%v", err)
	}
	if result.Token != (Token{}) {
		t.Fatalf("token delivered despite audit failure: %v", result.Token)
	}
	if sessions.revoked.Digest() != sessionToken.Digest() {
		t.Fatalf("revoked=%v want=%x", sessions.revoked, sessionToken.Digest())
	}
}

func TestMFAFactorLifecycleAuditsCommittedChanges(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	secretCipher, err := NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 22, 0, 0, 0, time.UTC)
	subjectID := uuid.MustNew()
	store := &mfaStoreStub{
		confirmResult:         true,
		rotationConfirmResult: true,
		disabledResult:        true,
	}
	auditor := &securityAuditorStub{}
	service, err := NewMFAService(
		store,
		&sessionStub{},
		secretCipher,
		WithMFAClock(func() time.Time { return now }),
		WithMFAAuditor(auditor),
	)
	if err != nil {
		t.Fatal(err)
	}

	enrollment, err := service.BeginTOTPEnrollment(context.Background(), subjectID, "Forge", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfirmTOTPEnrollment(context.Background(), subjectID, totpCode(raw, uint64(now.Unix()/totpPeriodSeconds))); err != nil {
		t.Fatal(err)
	}

	rotation, err := service.BeginTOTPRotation(context.Background(), subjectID, "Forge", "alice@example.com", changeAuthorizedBy(subjectID))
	if err != nil {
		t.Fatal(err)
	}
	rawRotation, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(rotation.Secret.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfirmTOTPRotation(context.Background(), subjectID, totpCode(rawRotation, uint64(now.Unix()/totpPeriodSeconds)), changeAuthorizedBy(subjectID)); err != nil {
		t.Fatal(err)
	}
	if err := service.DisableMFA(context.Background(), subjectID, changeAuthorizedBy(subjectID)); err != nil {
		t.Fatal(err)
	}

	want := []SecurityEventKind{
		SecurityMFAEnrollmentStarted,
		SecurityMFAEnabled,
		SecurityMFARotationStarted,
		SecurityMFARotated,
		SecurityMFADisabled,
	}
	if len(auditor.events) != len(want) {
		t.Fatalf("events=%+v", auditor.events)
	}
	for index, kind := range want {
		event := auditor.events[index]
		if event.Kind != kind || event.Outcome != SecurityOutcomeSucceeded ||
			event.SubjectID != subjectID || event.ActorID != subjectID {
			t.Fatalf("event[%d]=%+v want kind=%s", index, event, kind)
		}
	}
}
