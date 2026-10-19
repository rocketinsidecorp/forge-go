package auth

import (
	"context"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- RFC 6238 TOTP interoperability requires HMAC-SHA1 support.
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/rocketinsidecorp/forge-go/encryption"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

const (
	totpSecretBytes        = 20
	totpDigits             = 6
	totpPeriodSeconds      = 30
	defaultTOTPEnrollTTL   = 10 * time.Minute
	defaultMFAChallengeTTL = 5 * time.Minute
	minMFAChallengeTTL     = time.Minute
	maxMFAChallengeTTL     = 15 * time.Minute
)

var (
	ErrMFAInvalid           = fault.New("mfa_invalid", "multi-factor code or challenge is invalid or expired", fault.CategoryUnauthorized, 0)
	ErrMFAEnrollmentInvalid = fault.New("mfa_enrollment_invalid", "MFA enrollment is invalid or expired", fault.CategoryInvalid, 0)
	ErrMFAAlreadyEnabled    = fault.New("mfa_already_enabled", "MFA is already enabled", fault.CategoryConflict, 0)
	ErrMFANotEnabled        = fault.New("mfa_not_enabled", "MFA is not enabled", fault.CategoryConflict, 0)
)

type MFAChallengeToken struct {
	secret string
	digest Digest
}

func NewMFAChallengeToken() (MFAChallengeToken, error) {
	raw := make([]byte, tokenBytes)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return MFAChallengeToken{}, err
	}
	return MFAChallengeToken{
		secret: base64.RawURLEncoding.EncodeToString(raw),
		digest: sha256.Sum256(raw),
	}, nil
}

func ParseMFAChallengeToken(secret string) (MFAChallengeToken, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(secret)
	if err != nil || len(raw) != tokenBytes || base64.RawURLEncoding.EncodeToString(raw) != secret {
		return MFAChallengeToken{}, ErrMFAInvalid
	}
	return MFAChallengeToken{secret: secret, digest: sha256.Sum256(raw)}, nil
}

func (t MFAChallengeToken) Reveal() string { return t.secret }

func (MFAChallengeToken) String() string { return "[REDACTED]" }

func (MFAChallengeToken) GoString() string { return "auth.MFAChallengeToken([REDACTED])" }

func (MFAChallengeToken) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

func (t MFAChallengeToken) Digest() Digest { return t.digest }

// SecretCipher protects MFA seed material before it reaches persistence.
// Implementations must provide authenticated encryption.
type SecretCipher interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
}

type AESGCMSecretCipher struct {
	aead cipher.AEAD
}

// NewAESGCMSecretCipherBase64 decodes a canonical unpadded base64url key and
// builds the standard AES-256-GCM cipher. Errors never echo the secret value.
func NewAESGCMSecretCipherBase64(encoded string) (*AESGCMSecretCipher, error) {
	raw, err := encryption.ParseKey(encoded)
	if err != nil {
		return nil, errors.New("MFA encryption key must be canonical unpadded base64url for exactly 32 bytes")
	}
	return NewAESGCMSecretCipher(raw)
}

// NewAESGCMSecretCipher builds a standard-library AES-256-GCM secret cipher on
// the primitives of package encryption. The stored form is nonce||ciphertext
// with no associated data, unchanged since the first release. Applications
// must load the 32-byte key from an external secret manager or environment
// secret and must not store it in the identity database. Applications that
// need key rotation implement SecretCipher with encryption.Cipher instead.
func NewAESGCMSecretCipher(key []byte) (*AESGCMSecretCipher, error) {
	if len(key) != 32 {
		return nil, errors.New("MFA encryption key must be exactly 32 bytes")
	}
	aead, err := encryption.NewAEAD(key)
	if err != nil {
		return nil, err
	}
	return &AESGCMSecretCipher{aead: aead}, nil
}

func (c *AESGCMSecretCipher) Seal(plaintext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("MFA secret cipher is not configured")
	}
	return encryption.SealPrefixed(c.aead, plaintext, nil)
}

func (c *AESGCMSecretCipher) Open(ciphertext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("MFA secret cipher is not configured")
	}
	plaintext, err := encryption.OpenPrefixed(c.aead, ciphertext, nil)
	if err != nil {
		return nil, errors.New("invalid encrypted MFA secret")
	}
	return plaintext, nil
}

type TOTPSecretRecord struct {
	SubjectID        uuid.UUID
	SecretDigest     Digest
	SecretCiphertext []byte
	ExpiresAt        time.Time
	LastCounter      int64
}

type MFAChallengeRecord struct {
	SubjectID         uuid.UUID
	MembershipID      uuid.UUID
	CredentialVersion int64
	SecretDigest      Digest
	SecretCiphertext  []byte
	LastCounter       int64
	ExpiresAt         time.Time
	SessionExpiresAt  time.Time
}

// MFAStore persists pending/active TOTP factors and one-time login challenges.
// Confirm/Consume methods must compare the supplied secret digest and serialize
// updates so one TOTP counter cannot be accepted twice.
type MFAStore interface {
	BeginTOTPEnrollment(ctx context.Context, subjectID uuid.UUID, secretDigest Digest, secretCiphertext []byte, expiresAt time.Time) error
	LoadPendingTOTP(ctx context.Context, subjectID uuid.UUID) (record TOTPSecretRecord, found bool, err error)
	ConfirmTOTPEnrollment(ctx context.Context, subjectID uuid.UUID, secretDigest Digest, counter int64, backupCodeDigests []Digest) (confirmed bool, err error)

	CreateMFAChallenge(ctx context.Context, digest Digest, membershipID uuid.UUID, credentialVersion int64, expiresAt, sessionExpiresAt time.Time) error
	LoadMFAChallenge(ctx context.Context, digest Digest) (record MFAChallengeRecord, found bool, err error)
	ConsumeMFAChallenge(ctx context.Context, digest Digest, secretDigest Digest, counter int64) (membershipID uuid.UUID, consumed bool, err error)
	ConsumeMFABackupChallenge(ctx context.Context, digest Digest, backupCodeDigest Digest) (membershipID uuid.UUID, consumed bool, err error)
}

// MFAChallengeIssuer is the narrow LoginService dependency used after a
// correct password for an account that has MFA enabled.
type MFALifecycleStore interface {
	BeginTOTPRotation(ctx context.Context, subjectID uuid.UUID, secretDigest Digest, secretCiphertext []byte, expiresAt time.Time) error
	ConfirmTOTPRotation(ctx context.Context, subjectID uuid.UUID, secretDigest Digest, counter int64, backupCodeDigests []Digest) (confirmed bool, err error)
	DisableMFA(ctx context.Context, subjectID uuid.UUID) (disabled bool, err error)
}

type MFAChallengeIssuer interface {
	IssueMFAChallenge(ctx context.Context, membershipID uuid.UUID, credentialVersion int64, sessionExpiresAt time.Time) (token MFAChallengeToken, expiresAt time.Time, err error)
}

type TOTPSecret struct {
	value string
}

func (s TOTPSecret) Reveal() string { return s.value }

func (TOTPSecret) String() string { return "[REDACTED]" }

func (TOTPSecret) GoString() string { return "auth.TOTPSecret([REDACTED])" }

func (TOTPSecret) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

type TOTPProvisioningURI struct {
	value string
}

func (u TOTPProvisioningURI) Reveal() string { return u.value }

func (TOTPProvisioningURI) String() string { return "[REDACTED]" }

func (TOTPProvisioningURI) GoString() string {
	return "auth.TOTPProvisioningURI([REDACTED])"
}

func (TOTPProvisioningURI) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

type TOTPEnrollment struct {
	Secret          TOTPSecret
	ProvisioningURI TOTPProvisioningURI
	ExpiresAt       time.Time
}

type TOTPConfirmation struct {
	BackupCodes []MFABackupCode
}

type MFACompletion struct {
	ChallengeToken string
	Code           string
	Source         string
}

type MFAService struct {
	store        MFAStore
	sessions     CredentialSessionCreator
	cipher       SecretCipher
	throttler    LoginThrottler
	auditor      SecurityAuditor
	now          func() time.Time
	enrollTTL    time.Duration
	challengeTTL time.Duration
}

type MFAOption func(*MFAService) error

func WithMFAClock(now func() time.Time) MFAOption {
	return func(service *MFAService) error {
		if now == nil {
			return errors.New("MFA clock is required")
		}
		service.now = now
		return nil
	}
}

func WithMFAChallengeTTL(ttl time.Duration) MFAOption {
	return func(service *MFAService) error {
		if ttl < minMFAChallengeTTL || ttl > maxMFAChallengeTTL {
			return errors.New("MFA challenge TTL must be between 1 and 15 minutes")
		}
		service.challengeTTL = ttl
		return nil
	}
}

func WithMFAThrottler(throttler LoginThrottler) MFAOption {
	return func(service *MFAService) error {
		if throttler == nil {
			return errors.New("MFA throttler is required")
		}
		service.throttler = throttler
		return nil
	}
}

func WithMFAAuditor(auditor SecurityAuditor) MFAOption {
	return func(service *MFAService) error {
		if auditor == nil {
			return errors.New("MFA security auditor is required")
		}
		service.auditor = auditor
		return nil
	}
}

// DefaultMFAThrottleConfig returns the process-local MFA challenge throttle
// policy. Distributed deployments can use the same policy with a shared
// LoginThrottler adapter.
func DefaultMFAThrottleConfig() LoginThrottleConfig {
	return LoginThrottleConfig{
		Window:       defaultMFAChallengeTTL,
		AccountLimit: 5,
		SourceLimit:  60,
		MaxEntries:   20_000,
	}
}

func NewMFAService(store MFAStore, sessions CredentialSessionCreator, secretCipher SecretCipher, options ...MFAOption) (*MFAService, error) {
	if store == nil || sessions == nil || secretCipher == nil {
		return nil, errors.New("MFA store, session creator and secret cipher are required")
	}
	throttler, err := NewMemoryLoginThrottler(DefaultMFAThrottleConfig())
	if err != nil {
		return nil, err
	}
	service := &MFAService{
		store:        store,
		sessions:     sessions,
		cipher:       secretCipher,
		throttler:    throttler,
		now:          time.Now,
		enrollTTL:    defaultTOTPEnrollTTL,
		challengeTTL: defaultMFAChallengeTTL,
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil MFA option")
		}
		if err := option(service); err != nil {
			return nil, err
		}
	}
	return service, nil
}

// BeginTOTPEnrollment creates a new pending TOTP seed without replacing any
// already-active factor. The caller must be an authenticated account-management
// flow; this service intentionally does not infer identity from HTTP input.
func (s *MFAService) BeginTOTPEnrollment(ctx context.Context, subjectID uuid.UUID, issuer, account string) (TOTPEnrollment, error) {
	return s.beginTOTP(ctx, subjectID, issuer, account, false)
}

func (s *MFAService) BeginTOTPRotation(ctx context.Context, subjectID uuid.UUID, issuer, account string) (TOTPEnrollment, error) {
	return s.beginTOTP(ctx, subjectID, issuer, account, true)
}

func (s *MFAService) beginTOTP(ctx context.Context, subjectID uuid.UUID, issuer, account string, rotation bool) (TOTPEnrollment, error) {
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return TOTPEnrollment{}, errors.New("MFA subject ID must be a UUIDv7")
	}
	issuer = strings.TrimSpace(issuer)
	account = strings.TrimSpace(account)
	if !validTOTPLabel(issuer, 128) || !validTOTPLabel(account, 320) {
		return TOTPEnrollment{}, errors.New("TOTP issuer and account labels are required and must not contain control characters")
	}

	secret := make([]byte, totpSecretBytes)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return TOTPEnrollment{}, err
	}
	ciphertext, err := s.cipher.Seal(secret)
	if err != nil {
		return TOTPEnrollment{}, err
	}
	digest := sha256.Sum256(secret)
	expiresAt := s.now().UTC().Add(s.enrollTTL)
	if rotation {
		store, err := s.lifecycleStore()
		if err != nil {
			return TOTPEnrollment{}, err
		}
		if err := store.BeginTOTPRotation(ctx, subjectID, digest, ciphertext, expiresAt); err != nil {
			return TOTPEnrollment{}, err
		}
	} else {
		if err := s.store.BeginTOTPEnrollment(ctx, subjectID, digest, ciphertext, expiresAt); err != nil {
			return TOTPEnrollment{}, err
		}
	}

	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	query := url.Values{}
	query.Set("secret", encoded)
	query.Set("issuer", issuer)
	query.Set("algorithm", "SHA1")
	query.Set("digits", strconv.Itoa(totpDigits))
	query.Set("period", strconv.Itoa(totpPeriodSeconds))
	label := url.PathEscape(issuer + ":" + account)
	enrollment := TOTPEnrollment{
		Secret:          TOTPSecret{value: encoded},
		ProvisioningURI: TOTPProvisioningURI{value: "otpauth://totp/" + label + "?" + query.Encode()},
		ExpiresAt:       expiresAt,
	}
	kind := SecurityMFAEnrollmentStarted
	if rotation {
		kind = SecurityMFARotationStarted
	}
	auditErr := s.recordMFAEvent(ctx, SecurityEvent{
		Kind:      kind,
		Outcome:   SecurityOutcomeSucceeded,
		ActorID:   subjectID,
		SubjectID: subjectID,
	}, true)
	return enrollment, auditErr
}

func (s *MFAService) ConfirmTOTPEnrollment(ctx context.Context, subjectID uuid.UUID, code string) (TOTPConfirmation, error) {
	record, found, err := s.store.LoadPendingTOTP(ctx, subjectID)
	if err != nil {
		return TOTPConfirmation{}, err
	}
	now := s.now().UTC()
	if !found || record.ExpiresAt.IsZero() || !now.Before(record.ExpiresAt) {
		return TOTPConfirmation{}, ErrMFAEnrollmentInvalid
	}
	secret, err := s.openTOTPSecret(record.SecretCiphertext, record.SecretDigest)
	if err != nil {
		return TOTPConfirmation{}, err
	}
	counter, ok := matchTOTPCounter(secret, code, now, -1)
	if !ok {
		return TOTPConfirmation{}, ErrMFAEnrollmentInvalid
	}
	backupCodes, backupDigests, err := newMFABackupCodes(DefaultMFABackupCodeCount)
	if err != nil {
		return TOTPConfirmation{}, err
	}
	confirmed, err := s.store.ConfirmTOTPEnrollment(ctx, subjectID, record.SecretDigest, counter, backupDigests)
	if err != nil {
		return TOTPConfirmation{}, err
	}
	if !confirmed {
		return TOTPConfirmation{}, ErrMFAEnrollmentInvalid
	}
	confirmation := TOTPConfirmation{BackupCodes: backupCodes}
	auditErr := s.recordMFAEvent(ctx, SecurityEvent{
		Kind:      SecurityMFAEnabled,
		Outcome:   SecurityOutcomeSucceeded,
		ActorID:   subjectID,
		SubjectID: subjectID,
	}, true)
	return confirmation, auditErr
}

func (s *MFAService) ConfirmTOTPRotation(ctx context.Context, subjectID uuid.UUID, code string) (TOTPConfirmation, error) {
	record, found, err := s.store.LoadPendingTOTP(ctx, subjectID)
	if err != nil {
		return TOTPConfirmation{}, err
	}
	now := s.now().UTC()
	if !found || record.ExpiresAt.IsZero() || !now.Before(record.ExpiresAt) {
		return TOTPConfirmation{}, ErrMFAEnrollmentInvalid
	}
	secret, err := s.openTOTPSecret(record.SecretCiphertext, record.SecretDigest)
	if err != nil {
		return TOTPConfirmation{}, err
	}
	counter, ok := matchTOTPCounter(secret, code, now, -1)
	if !ok {
		return TOTPConfirmation{}, ErrMFAEnrollmentInvalid
	}
	backupCodes, backupDigests, err := newMFABackupCodes(DefaultMFABackupCodeCount)
	if err != nil {
		return TOTPConfirmation{}, err
	}
	store, err := s.lifecycleStore()
	if err != nil {
		return TOTPConfirmation{}, err
	}
	confirmed, err := store.ConfirmTOTPRotation(ctx, subjectID, record.SecretDigest, counter, backupDigests)
	if err != nil {
		return TOTPConfirmation{}, err
	}
	if !confirmed {
		return TOTPConfirmation{}, ErrMFAEnrollmentInvalid
	}
	confirmation := TOTPConfirmation{BackupCodes: backupCodes}
	auditErr := s.recordMFAEvent(ctx, SecurityEvent{
		Kind:      SecurityMFARotated,
		Outcome:   SecurityOutcomeSucceeded,
		ActorID:   subjectID,
		SubjectID: subjectID,
	}, true)
	return confirmation, auditErr
}

func (s *MFAService) DisableMFA(ctx context.Context, subjectID uuid.UUID) error {
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return errors.New("MFA subject ID must be a UUIDv7")
	}
	store, err := s.lifecycleStore()
	if err != nil {
		return err
	}
	disabled, err := store.DisableMFA(ctx, subjectID)
	if err != nil {
		return err
	}
	if !disabled {
		return ErrMFANotEnabled
	}
	return s.recordMFAEvent(ctx, SecurityEvent{
		Kind:      SecurityMFADisabled,
		Outcome:   SecurityOutcomeSucceeded,
		ActorID:   subjectID,
		SubjectID: subjectID,
	}, true)
}

func (s *MFAService) recordMFAEvent(ctx context.Context, event SecurityEvent, operationApplied bool) error {
	if s.auditor == nil {
		return nil
	}
	return securityAuditError(s.auditor.RecordSecurityEvent(ctx, event), operationApplied)
}

func (s *MFAService) lifecycleStore() (MFALifecycleStore, error) {
	store, ok := s.store.(MFALifecycleStore)
	if !ok {
		return nil, errors.New("MFA lifecycle is not supported by the configured store")
	}
	return store, nil
}

func (s *MFAService) IssueMFAChallenge(ctx context.Context, membershipID uuid.UUID, credentialVersion int64, sessionExpiresAt time.Time) (MFAChallengeToken, time.Time, error) {
	if membershipID.Version() != 7 || membershipID.Variant() != 2 {
		return MFAChallengeToken{}, time.Time{}, errors.New("MFA membership ID must be a UUIDv7")
	}
	if credentialVersion < 1 {
		return MFAChallengeToken{}, time.Time{}, errors.New("MFA credential version must be positive")
	}
	now := s.now().UTC()
	if !sessionExpiresAt.After(now) {
		return MFAChallengeToken{}, time.Time{}, errors.New("MFA session expiry must be in the future")
	}
	token, err := NewMFAChallengeToken()
	if err != nil {
		return MFAChallengeToken{}, time.Time{}, err
	}
	expiresAt := now.Add(s.challengeTTL)
	if err := s.store.CreateMFAChallenge(ctx, token.Digest(), membershipID, credentialVersion, expiresAt, sessionExpiresAt.UTC()); err != nil {
		return MFAChallengeToken{}, time.Time{}, err
	}
	return token, expiresAt, nil
}

// CompleteLogin verifies one TOTP code, atomically consumes the challenge and
// TOTP counter, then creates the normal opaque session. If session persistence
// fails after consumption, the caller must start a fresh password login; the
// challenge is never resurrected.
func (s *MFAService) CompleteLogin(ctx context.Context, completion MFACompletion) (LoginResult, error) {
	token, err := ParseMFAChallengeToken(strings.TrimSpace(completion.ChallengeToken))
	if err != nil {
		return LoginResult{}, ErrMFAInvalid
	}
	key := deriveMFAThrottleKey(token.Digest(), strings.TrimSpace(completion.Source))
	decision, err := s.throttler.Attempt(ctx, key)
	if err != nil {
		return LoginResult{}, err
	}
	if !decision.Allowed {
		throttled := &LoginThrottledError{RetryAfter: decision.RetryAfter}
		auditErr := s.recordMFAChallengeFailure(ctx, MFAChallengeRecord{}, token.Digest(), key, SecurityOutcomeDenied)
		return LoginResult{}, errors.Join(throttled, auditErr)
	}

	record, found, err := s.store.LoadMFAChallenge(ctx, token.Digest())
	if err != nil {
		return LoginResult{}, err
	}
	now := s.now().UTC()
	if !found || record.ExpiresAt.IsZero() || !now.Before(record.ExpiresAt) ||
		record.SessionExpiresAt.IsZero() || !now.Before(record.SessionExpiresAt) {
		auditErr := s.recordMFAChallengeFailure(ctx, record, token.Digest(), key, SecurityOutcomeDenied)
		return LoginResult{}, errors.Join(ErrMFAInvalid, auditErr)
	}

	var (
		membershipID uuid.UUID
		consumed     bool
		usedBackup   bool
	)
	if backupDigest, backup := parseMFABackupCode(completion.Code); backup {
		usedBackup = true
		membershipID, consumed, err = s.store.ConsumeMFABackupChallenge(ctx, token.Digest(), backupDigest)
		if err != nil {
			return LoginResult{}, err
		}
	} else {
		secret, err := s.openTOTPSecret(record.SecretCiphertext, record.SecretDigest)
		if err != nil {
			auditErr := s.recordMFAChallengeFailure(ctx, record, token.Digest(), key, SecurityOutcomeFailed)
			return LoginResult{}, errors.Join(err, auditErr)
		}
		counter, ok := matchTOTPCounter(secret, completion.Code, now, record.LastCounter)
		if !ok {
			auditErr := s.recordMFAChallengeFailure(ctx, record, token.Digest(), key, SecurityOutcomeDenied)
			return LoginResult{}, errors.Join(ErrMFAInvalid, auditErr)
		}
		membershipID, consumed, err = s.store.ConsumeMFAChallenge(ctx, token.Digest(), record.SecretDigest, counter)
		if err != nil {
			return LoginResult{}, err
		}
	}
	if !consumed {
		auditErr := s.recordMFAChallengeFailure(ctx, record, token.Digest(), key, SecurityOutcomeDenied)
		return LoginResult{}, errors.Join(ErrMFAInvalid, auditErr)
	}
	if err := s.throttler.Success(ctx, key); err != nil {
		return LoginResult{}, err
	}

	expiresAt := record.SessionExpiresAt.UTC()
	session, err := s.sessions.CreateSessionAtVersion(ctx, membershipID, record.CredentialVersion, expiresAt)
	if err != nil {
		return LoginResult{}, err
	}

	if usedBackup {
		event := SecurityEvent{
			Kind:             SecurityMFABackupCodeUsed,
			Outcome:          SecurityOutcomeSucceeded,
			ActorID:          record.SubjectID,
			SubjectID:        record.SubjectID,
			MembershipID:     membershipID,
			CredentialDigest: token.Digest(),
		}
		if key.HasSource {
			event.SourceDigest = Digest(key.Source)
		}
		if auditErr := s.recordMFAEvent(ctx, event, false); auditErr != nil {
			return LoginResult{}, s.rejectMFASessionAfterAuditFailure(ctx, session, auditErr)
		}
	}

	loginEvent := SecurityEvent{
		Kind:             SecurityLoginSucceeded,
		Outcome:          SecurityOutcomeSucceeded,
		SubjectID:        record.SubjectID,
		MembershipID:     membershipID,
		CredentialDigest: session.Digest(),
	}
	if key.HasSource {
		loginEvent.SourceDigest = Digest(key.Source)
	}
	if auditErr := s.recordMFAEvent(ctx, loginEvent, false); auditErr != nil {
		return LoginResult{}, s.rejectMFASessionAfterAuditFailure(ctx, session, auditErr)
	}

	return LoginResult{Token: session, ExpiresAt: expiresAt}, nil
}

func (s *MFAService) recordMFAChallengeFailure(
	ctx context.Context,
	record MFAChallengeRecord,
	challengeDigest Digest,
	key LoginThrottleKey,
	outcome SecurityOutcome,
) error {
	event := SecurityEvent{
		Kind:             SecurityMFAChallengeFailed,
		Outcome:          outcome,
		SubjectID:        record.SubjectID,
		MembershipID:     record.MembershipID,
		CredentialDigest: challengeDigest,
	}
	if key.HasSource {
		event.SourceDigest = Digest(key.Source)
	}
	return s.recordMFAEvent(ctx, event, false)
}

func (s *MFAService) rejectMFASessionAfterAuditFailure(ctx context.Context, session Token, auditErr error) error {
	if auditErr == nil {
		return nil
	}
	if revoker, ok := s.sessions.(SessionRevoker); ok {
		return errors.Join(auditErr, revoker.RevokeSession(ctx, session))
	}
	return auditErr
}

func (s *MFAService) openTOTPSecret(ciphertext []byte, wantDigest Digest) ([]byte, error) {
	secret, err := s.cipher.Open(ciphertext)
	if err != nil {
		return nil, err
	}
	if len(secret) != totpSecretBytes {
		return nil, errors.New("decrypted MFA secret has invalid length")
	}
	got := sha256.Sum256(secret)
	if subtle.ConstantTimeCompare(got[:], wantDigest[:]) != 1 {
		return nil, errors.New("decrypted MFA secret digest mismatch")
	}
	return secret, nil
}

func matchTOTPCounter(secret []byte, code string, now time.Time, lastCounter int64) (int64, bool) {
	if len(secret) != totpSecretBytes || len(code) != totpDigits {
		return 0, false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	current := now.Unix() / totpPeriodSeconds
	if current < 0 {
		return 0, false
	}
	for _, delta := range []int64{0, -1, 1} {
		counter := current + delta
		if counter < 0 || counter <= lastCounter {
			continue
		}
		expected := totpCode(secret, uint64(counter))
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			return counter, true
		}
	}
	return 0, false
}

func totpCode(secret []byte, counter uint64) string {
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], counter)
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write(message[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	binaryCode := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])
	value := binaryCode % 1_000_000
	return fmt.Sprintf("%06d", value)
}

func validTOTPLabel(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes {
		return false
	}
	return !strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsControl(r) || r == '\u007f'
	})
}

func deriveMFAThrottleKey(digest Digest, source string) LoginThrottleKey {
	key := LoginThrottleKey{
		Account: sha256.Sum256(append([]byte("mfa-challenge\x00"), digest[:]...)),
	}
	if source != "" {
		key.Source = sha256.Sum256([]byte("mfa-source\x00" + source))
		key.HasSource = true
	}
	return key
}
