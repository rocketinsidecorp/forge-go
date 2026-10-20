package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
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

	// ErrRecoveryQueueFull is returned by a RecoveryDispatcher that holds as
	// much work as it is allowed to: the request is dropped and logged, never
	// queued without bound and never made to wait.
	ErrRecoveryQueueFull = errors.New("password recovery queue is full")
	// ErrRecoveryQueueClosed is returned by a RecoveryDispatcher that has
	// stopped accepting work (the application is shutting down).
	ErrRecoveryQueueClosed = errors.New("password recovery queue is closed")
)

// DefaultRecoveryResponseTime is the least time RecoveryService.Request takes
// to answer.
const DefaultRecoveryResponseTime = 100 * time.Millisecond

const maxRecoveryResponseTime = 5 * time.Second

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

// RecoveryTokenChecker is an optional RecoveryStore capability: it reports
// whether a token digest names a recovery that exists, is unused and has not
// expired. RecoveryService.Reset asks it before hashing the new password, so
// a request with an unknown, used or expired token costs a lookup, not a
// password hash. auth/postgres.Repository implements it; a store without it
// still works and hashes first.
type RecoveryTokenChecker interface {
	RecoveryTokenUsable(ctx context.Context, digest Digest) (bool, error)
}

// RecoveryJob is the work of one password-recovery request, once the request
// has been accepted: a canonical login and tenant slug, whether or not they
// name an account, and the throttle key (digests only) the request was
// counted under, for the audit event. It carries no token: RecoveryService
// issues the token when it processes the job, and the token never leaves the
// process except through the RecoverySender.
type RecoveryJob struct {
	Email      string
	TenantSlug string
	Key        LoginThrottleKey
}

// RecoveryDispatcher takes accepted requests off the request path. When a
// RecoveryService has one, Request answers without looking the account up,
// issuing a token or delivering anything, and the dispatcher arranges for
// RecoveryService.Process to run: the in-process RecoveryQueue, or the job
// queue adapter auth/recoveryqueue. Implementations must not block: they
// return ErrRecoveryQueueFull instead.
type RecoveryDispatcher interface {
	DispatchRecovery(ctx context.Context, job RecoveryJob) error
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
	store        RecoveryStore
	sender       RecoverySender
	throttler    LoginThrottler
	auditor      SecurityAuditor
	dispatcher   RecoveryDispatcher
	logger       *slog.Logger
	now          func() time.Time
	ttl          time.Duration
	responseTime time.Duration
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

// WithRecoveryDispatcher moves issuing and delivering reset links off the
// request path. Without it, Request does that work inline, so an existing
// account answers more slowly than an unknown one (the minimum response time
// only hides the difference while delivery is faster than that). Set it
// wherever the request is answered to the public.
func WithRecoveryDispatcher(dispatcher RecoveryDispatcher) RecoveryOption {
	return func(service *RecoveryService) error {
		if dispatcher == nil {
			return errors.New("recovery dispatcher is required")
		}
		service.dispatcher = dispatcher
		return nil
	}
}

// WithRecoveryLogger sets the logger for requests dropped by a full or closed
// dispatcher. Nothing identifying (login, tenant, source, token) is logged.
// The default is slog.Default().
func WithRecoveryLogger(logger *slog.Logger) RecoveryOption {
	return func(service *RecoveryService) error {
		if logger == nil {
			return errors.New("recovery logger is required")
		}
		service.logger = logger
		return nil
	}
}

// WithRecoveryResponseTime sets the least time Request takes to answer
// (default DefaultRecoveryResponseTime, at most 5 seconds). Zero disables
// the floor; use that only in tests.
func WithRecoveryResponseTime(d time.Duration) RecoveryOption {
	return func(service *RecoveryService) error {
		if d < 0 || d > maxRecoveryResponseTime {
			return errors.New("recovery response time must be between 0 and 5 seconds")
		}
		service.responseTime = d
		return nil
	}
}

// DefaultRecoveryThrottleConfig is the password-recovery policy: in one hour,
// 3 requests per login and source, 10 per login (so a victim's mailbox cannot
// be flooded from many sources) and 30 per source, with at most 20,000
// in-memory counters.
func DefaultRecoveryThrottleConfig() LoginThrottleConfig {
	return LoginThrottleConfig{
		Window:       time.Hour,
		PairLimit:    3,
		AccountLimit: 10,
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
		store:        store,
		sender:       sender,
		throttler:    throttler,
		logger:       slog.Default(),
		now:          time.Now,
		ttl:          defaultRecoveryTTL,
		responseTime: DefaultRecoveryResponseTime,
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

// Request counts the request against the throttle, hands accepted work to
// the dispatcher and answers no sooner than the response time (100 ms by
// default). Syntactically invalid and unknown identities are accepted no-ops
// for public semantics, and so are accepted requests a full or closed
// dispatcher drops (logged). Infrastructure, throttling and enqueue failures
// remain errors; they do not depend on whether the account exists.
//
// Without a dispatcher the request is processed inline (see Process), and
// then delivery failures, and audit-persistence failures with auditing
// configured, are returned too; the HTTP adapter masks those account-dependent
// failures as the same 202 response. With a dispatcher they happen later,
// in Process.
func (s *RecoveryService) Request(ctx context.Context, request RecoveryRequest) error {
	started := time.Now()
	err := s.request(ctx, request)
	s.waitUntil(ctx, started.Add(s.responseTime))
	return err
}

func (s *RecoveryService) request(ctx context.Context, request RecoveryRequest) error {
	login, loginOK := CanonicalLogin(request.Email)
	tenant, tenantOK := canonicalTenant(request.TenantSlug)
	account := login
	if !loginOK {
		account = invalidLoginAccount(request.Email)
	}

	key := deriveRecoveryThrottleKey(account, request.Source)
	decision, err := s.throttler.Attempt(ctx, key)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		throttled := &RecoveryThrottledError{RetryAfter: decision.RetryAfter}
		auditErr := s.recordRecoveryRequestEvent(ctx, SecurityOutcomeDenied, uuid.UUID{}, key, Digest{}, false)
		return errors.Join(throttled, auditErr)
	}
	if !loginOK || !tenantOK {
		// Names no account: nothing to look up, issue or deliver.
		return nil
	}

	job := RecoveryJob{Email: login, TenantSlug: tenant, Key: key}
	if s.dispatcher == nil {
		return s.Process(ctx, job)
	}
	err = s.dispatcher.DispatchRecovery(ctx, job)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrRecoveryQueueFull), errors.Is(err, ErrRecoveryQueueClosed):
		s.logger.WarnContext(ctx, "password recovery request dropped", "reason", err.Error())
		return nil
	default:
		return fmt.Errorf("dispatch password recovery: %w", err)
	}
}

// Process looks up the identity of an accepted request and, when it names an
// active account, issues a token and delivers it to the address the
// credential store holds for that account, never to the address a visitor
// typed. Unknown identities are a no-op (audited as denied). Run it from a
// RecoveryDispatcher's worker, or let Request run it inline. Delivery failures
// invalidate the token and are returned as *RecoveryDeliveryError, so a job
// queue can retry; a retry issues a new token and invalidates the previous
// one, so only the newest link works.
func (s *RecoveryService) Process(ctx context.Context, job RecoveryJob) error {
	email, tenant, ok := normalizeLogin(job.Email, job.TenantSlug)
	if !ok {
		return nil
	}
	key := job.Key
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

// waitUntil blocks until deadline (wall clock) or until ctx ends.
func (s *RecoveryService) waitUntil(ctx context.Context, deadline time.Time) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

func (s *RecoveryService) Reset(ctx context.Context, reset PasswordReset) error {
	token, err := ParseRecoveryToken(strings.TrimSpace(reset.Token))
	if err != nil {
		return ErrRecoveryInvalid
	}
	key := deriveRecoveryResetThrottleKey(token.Digest(), reset.Source)
	decision, err := s.throttler.Attempt(ctx, key)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		throttled := &RecoveryThrottledError{RetryAfter: decision.RetryAfter}
		auditErr := s.recordPasswordResetEvent(ctx, SecurityOutcomeDenied, key, token.Digest(), false)
		return errors.Join(throttled, auditErr)
	}
	if checker, ok := s.store.(RecoveryTokenChecker); ok {
		// An unknown, used or expired token ends here, before the expensive
		// password hash an unauthenticated visitor could otherwise demand.
		usable, err := checker.RecoveryTokenUsable(ctx, token.Digest())
		if err != nil {
			return err
		}
		if !usable {
			auditErr := s.recordPasswordResetEvent(ctx, SecurityOutcomeDenied, key, token.Digest(), false)
			return errors.Join(ErrRecoveryInvalid, auditErr)
		}
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

// deriveRecoveryThrottleKey counts reset requests per login and source, per
// login across sources (which also caps the mail one victim can be flooded
// with) and per source.
func deriveRecoveryThrottleKey(email, source string) LoginThrottleKey {
	return newThrottleKey("recovery", email, source, true)
}

// deriveRecoveryResetThrottleKey counts reset submissions per token and per
// source. The token already names the account, so there is no pair counter.
func deriveRecoveryResetThrottleKey(digest Digest, source string) LoginThrottleKey {
	return newThrottleKey("recovery-reset", string(digest[:]), source, false)
}
