package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

const (
	defaultRecoveryTTL = 30 * time.Minute
	minRecoveryTTL     = 5 * time.Minute
	maxRecoveryTTL     = 24 * time.Hour
	recoveryCleanupTTL = 5 * time.Second
)

var (
	ErrInvalidRecoveryToken = errors.New("invalid password recovery token")
	ErrRecoveryInvalid      = fault.New("recovery_invalid", "recovery token is invalid or expired", fault.CategoryInvalid, 0)
	ErrRecoveryThrottled    = fault.New("recovery_throttled", "too many recovery requests", fault.CategoryRateLimited, 0)
	ErrPasswordInvalid      = fault.New("password_invalid", "password is invalid", fault.CategoryInvalid, 0)
	ErrRecoveryDelivery     = errors.New("password recovery delivery failed")
)

type RecoveryToken struct {
	secret string
	digest Digest
}

func NewRecoveryToken() (RecoveryToken, error) {
	return newRecoveryTokenWithEntropy(rand.Reader)
}

func newRecoveryTokenWithEntropy(entropy io.Reader) (RecoveryToken, error) {
	if entropy == nil {
		return RecoveryToken{}, errors.New("recovery token entropy source is required")
	}
	raw := make([]byte, tokenBytes)
	if _, err := io.ReadFull(entropy, raw); err != nil {
		return RecoveryToken{}, err
	}
	return RecoveryToken{
		secret: base64.RawURLEncoding.EncodeToString(raw),
		digest: sha256.Sum256(raw),
	}, nil
}

func ParseRecoveryToken(secret string) (RecoveryToken, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(secret)
	if err != nil || len(raw) != tokenBytes || base64.RawURLEncoding.EncodeToString(raw) != secret {
		return RecoveryToken{}, ErrInvalidRecoveryToken
	}
	return RecoveryToken{secret: secret, digest: sha256.Sum256(raw)}, nil
}

func (t RecoveryToken) Reveal() string { return t.secret }

func (RecoveryToken) String() string { return "[REDACTED]" }

func (RecoveryToken) GoString() string { return "auth.RecoveryToken([REDACTED])" }

func (RecoveryToken) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

func (t RecoveryToken) Digest() Digest { return t.digest }

type RecoveryIdentity struct {
	SubjectID uuid.UUID
	Email     string
}

type RecoveryStore interface {
	LookupRecovery(ctx context.Context, emailNormalized, tenantSlug string) (identity RecoveryIdentity, found bool, err error)
	CreateRecovery(ctx context.Context, subjectID uuid.UUID, digest Digest, expiresAt time.Time) error
	InvalidateRecovery(ctx context.Context, digest Digest) error
	ConsumeRecovery(ctx context.Context, digest Digest, nextPasswordHash string) (consumed bool, err error)
}

type PasswordRecoveryMessage struct {
	Email      string
	TenantSlug string
	Token      RecoveryToken
	ExpiresAt  time.Time
}

// RecoverySender delivers a reset message. Implementations should enqueue
// delivery and return promptly rather than perform slow SMTP/network delivery
// inline, so public request timing does not reveal whether an identity exists.
type RecoverySender interface {
	SendPasswordRecovery(ctx context.Context, message PasswordRecoveryMessage) error
}

type RecoverySenderFunc func(context.Context, PasswordRecoveryMessage) error

func (fn RecoverySenderFunc) SendPasswordRecovery(ctx context.Context, message PasswordRecoveryMessage) error {
	return fn(ctx, message)
}

type RecoveryRequest struct {
	Email      string
	TenantSlug string
	Source     string
}

type PasswordReset struct {
	Token       string
	NewPassword string
	Source      string
}

type RecoveryService struct {
	store     RecoveryStore
	sender    RecoverySender
	throttler LoginThrottler
	auditor   SecurityAuditor
	now       func() time.Time
	ttl       time.Duration
}

type RecoveryOption func(*RecoveryService) error

func WithRecoveryTTL(ttl time.Duration) RecoveryOption {
	return func(service *RecoveryService) error {
		if ttl < minRecoveryTTL || ttl > maxRecoveryTTL {
			return errors.New("recovery TTL must be between 5 minutes and 24 hours")
		}
		service.ttl = ttl
		return nil
	}
}

func WithRecoveryClock(now func() time.Time) RecoveryOption {
	return func(service *RecoveryService) error {
		if now == nil {
			return errors.New("recovery clock is required")
		}
		service.now = now
		return nil
	}
}

func WithRecoveryThrottler(throttler LoginThrottler) RecoveryOption {
	return func(service *RecoveryService) error {
		if throttler == nil {
			return errors.New("recovery throttler is required")
		}
		service.throttler = throttler
		return nil
	}
}

func WithRecoveryAuditor(auditor SecurityAuditor) RecoveryOption {
	return func(service *RecoveryService) error {
		if auditor == nil {
			return errors.New("recovery security auditor is required")
		}
		service.auditor = auditor
		return nil
	}
}

func DefaultRecoveryThrottleConfig() LoginThrottleConfig {
	return LoginThrottleConfig{
		Window:       time.Hour,
		AccountLimit: 3,
		SourceLimit:  30,
		MaxEntries:   20_000,
	}
}

func NewRecoveryService(store RecoveryStore, sender RecoverySender, options ...RecoveryOption) (*RecoveryService, error) {
	if store == nil || sender == nil {
		return nil, errors.New("recovery store and sender are required")
	}
	throttler, err := NewMemoryLoginThrottler(DefaultRecoveryThrottleConfig())
	if err != nil {
		return nil, err
	}
	service := &RecoveryService{
		store:     store,
		sender:    sender,
		throttler: throttler,
		now:       time.Now,
		ttl:       defaultRecoveryTTL,
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil recovery option")
		}
		if err := option(service); err != nil {
			return nil, err
		}
	}
	return service, nil
}

// Request treats syntactically invalid and unknown identities as accepted
// no-ops for public semantics. With auditing configured, an audit-persistence
// failure may still be returned to non-HTTP callers; the HTTP adapter masks
// that account-dependent failure as the same 202 response. Infrastructure,
// throttling, persistence and delivery failures remain errors.
func (s *RecoveryService) Request(ctx context.Context, request RecoveryRequest) error {
	email, tenant, ok := normalizeLogin(request.Email, request.TenantSlug)
	if !ok {
		return nil
	}

	key := deriveRecoveryThrottleKey(email, tenant, strings.TrimSpace(request.Source))
	decision, err := s.throttler.Attempt(ctx, key)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		throttled := &RecoveryThrottledError{RetryAfter: decision.RetryAfter}
		auditErr := s.recordRecoveryRequestEvent(ctx, SecurityOutcomeDenied, uuid.UUID{}, key, Digest{}, false)
		return errors.Join(throttled, auditErr)
	}

	identity, found, err := s.store.LookupRecovery(ctx, email, tenant)
	if err != nil {
		return err
	}
	if !found {
		return s.recordRecoveryRequestEvent(ctx, SecurityOutcomeDenied, uuid.UUID{}, key, Digest{}, false)
	}
	if identity.SubjectID.Version() != 7 || identity.SubjectID.Variant() != 2 || identity.Email == "" {
		return errors.New("recovery store returned an invalid identity")
	}

	token, err := NewRecoveryToken()
	if err != nil {
		return err
	}
	expiresAt := s.now().UTC().Add(s.ttl)
	if err := s.store.CreateRecovery(ctx, identity.SubjectID, token.Digest(), expiresAt); err != nil {
		return err
	}
	message := PasswordRecoveryMessage{
		Email:      identity.Email,
		TenantSlug: tenant,
		Token:      token,
		ExpiresAt:  expiresAt,
	}
	if err := s.sender.SendPasswordRecovery(ctx, message); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recoveryCleanupTTL)
		defer cancel()
		invalidateErr := s.store.InvalidateRecovery(cleanupCtx, token.Digest())
		deliveryErr := &RecoveryDeliveryError{Cause: errors.Join(err, invalidateErr)}
		auditErr := s.recordRecoveryRequestEvent(ctx, SecurityOutcomeFailed, identity.SubjectID, key, token.Digest(), false)
		return errors.Join(deliveryErr, auditErr)
	}
	return s.recordRecoveryRequestEvent(ctx, SecurityOutcomeSucceeded, identity.SubjectID, key, token.Digest(), true)
}

func (s *RecoveryService) Reset(ctx context.Context, reset PasswordReset) error {
	token, err := ParseRecoveryToken(strings.TrimSpace(reset.Token))
	if err != nil {
		return ErrRecoveryInvalid
	}
	key := deriveRecoveryResetThrottleKey(token.Digest(), strings.TrimSpace(reset.Source))
	decision, err := s.throttler.Attempt(ctx, key)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		throttled := &RecoveryThrottledError{RetryAfter: decision.RetryAfter}
		auditErr := s.recordPasswordResetEvent(ctx, SecurityOutcomeDenied, key, token.Digest(), false)
		return errors.Join(throttled, auditErr)
	}
	nextHash, err := HashPassword(reset.NewPassword)
	if err != nil {
		if errors.Is(err, ErrPasswordRequired) || errors.Is(err, ErrPasswordTooLong) {
			auditErr := s.recordPasswordResetEvent(ctx, SecurityOutcomeDenied, key, token.Digest(), false)
			return errors.Join(ErrPasswordInvalid, auditErr)
		}
		return err
	}
	consumed, err := s.store.ConsumeRecovery(ctx, token.Digest(), nextHash)
	if err != nil {
		return err
	}
	if !consumed {
		auditErr := s.recordPasswordResetEvent(ctx, SecurityOutcomeDenied, key, token.Digest(), false)
		return errors.Join(ErrRecoveryInvalid, auditErr)
	}
	return s.recordPasswordResetEvent(ctx, SecurityOutcomeSucceeded, key, token.Digest(), true)
}

func (s *RecoveryService) recordRecoveryRequestEvent(
	ctx context.Context,
	outcome SecurityOutcome,
	subjectID uuid.UUID,
	key LoginThrottleKey,
	credentialDigest Digest,
	operationApplied bool,
) error {
	if s.auditor == nil {
		return nil
	}
	event := SecurityEvent{
		Kind:             SecurityRecoveryRequested,
		Outcome:          outcome,
		SubjectID:        subjectID,
		AccountDigest:    Digest(key.Account),
		CredentialDigest: credentialDigest,
	}
	if key.HasSource {
		event.SourceDigest = Digest(key.Source)
	}
	return securityAuditError(s.auditor.RecordSecurityEvent(ctx, event), operationApplied)
}

func (s *RecoveryService) recordPasswordResetEvent(
	ctx context.Context,
	outcome SecurityOutcome,
	key LoginThrottleKey,
	credentialDigest Digest,
	operationApplied bool,
) error {
	if s.auditor == nil {
		return nil
	}
	event := SecurityEvent{
		Kind:             SecurityPasswordReset,
		Outcome:          outcome,
		CredentialDigest: credentialDigest,
	}
	if key.HasSource {
		event.SourceDigest = Digest(key.Source)
	}
	return securityAuditError(s.auditor.RecordSecurityEvent(ctx, event), operationApplied)
}

type RecoveryDeliveryError struct {
	Cause error
}

func (e *RecoveryDeliveryError) Error() string { return ErrRecoveryDelivery.Error() }

func (e *RecoveryDeliveryError) Unwrap() error {
	if e == nil || e.Cause == nil {
		return ErrRecoveryDelivery
	}
	return errors.Join(ErrRecoveryDelivery, e.Cause)
}

type RecoveryThrottledError struct {
	RetryAfter time.Duration
}

func (e *RecoveryThrottledError) Error() string { return ErrRecoveryThrottled.Error() }

func (e *RecoveryThrottledError) Unwrap() error { return ErrRecoveryThrottled }

func deriveRecoveryThrottleKey(email, tenant, source string) LoginThrottleKey {
	key := LoginThrottleKey{
		Account: sha256.Sum256([]byte("recovery-account\x00" + tenant + "\x00" + email)),
	}
	if source != "" {
		key.Source = sha256.Sum256([]byte("recovery-source\x00" + source))
		key.HasSource = true
	}
	return key
}

func deriveRecoveryResetThrottleKey(digest Digest, source string) LoginThrottleKey {
	key := LoginThrottleKey{
		Account: sha256.Sum256(append([]byte("recovery-reset\x00"), digest[:]...)),
	}
	if source != "" {
		key.Source = sha256.Sum256([]byte("recovery-reset-source\x00" + source))
		key.HasSource = true
	}
	return key
}
