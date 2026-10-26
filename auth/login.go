package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

const (
	defaultSessionTTL = 24 * time.Hour
	minSessionTTL     = 5 * time.Minute
	maxSessionTTL     = 30 * 24 * time.Hour

	// A syntactically valid hash with an impossible all-zero derived key.
	// Missing identities still execute PBKDF2 so account/tenant misses do not
	// take a dramatically cheaper path than a wrong password.
	dummyPasswordHash = "$pbkdf2-sha256$v=1$i=600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // #nosec G101 -- an all-zero derived key is a public timing-equalizer, not a credential
)

var tenantSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// PasswordIdentity is the minimum server-side state needed to verify a login.
// PasswordHash must never be returned to clients or logs.
type PasswordIdentity struct {
	SubjectID         uuid.UUID
	MembershipID      uuid.UUID
	PasswordHash      string
	CredentialVersion int64
	MFARequired       bool
}

// PasswordStore loads current credential state and conditionally upgrades a
// password hash. ReplacePasswordHash must compare previousHash atomically and
// return replaced=false if the credential changed concurrently.
type PasswordStore interface {
	LookupPassword(ctx context.Context, emailNormalized, tenantSlug string) (identity PasswordIdentity, found bool, err error)
	ReplacePasswordHash(ctx context.Context, subjectID uuid.UUID, previousHash, nextHash string) (replaced bool, err error)
}

// SessionCreator persists a new session while revalidating the membership and
// account state. The returned Token is the one-time plaintext credential.
type SessionCreator interface {
	CreateSession(ctx context.Context, membershipID uuid.UUID, expiresAt time.Time) (Token, error)
}

// CredentialSessionCreator creates a session only if credentialVersion is still
// current. Password login and MFA use this to close the race between verifying
// credentials and persisting the resulting session.
type CredentialSessionCreator interface {
	CreateSessionAtVersion(ctx context.Context, membershipID uuid.UUID, credentialVersion int64, expiresAt time.Time) (Token, error)
}

// SessionRevoker invalidates one opaque session credential.
type SessionRevoker interface {
	RevokeSession(ctx context.Context, token Token) error
}

type LoginInput struct {
	Email      string
	Password   string
	TenantSlug string
	Source     string
}

type LoginResult struct {
	Token        Token
	ExpiresAt    time.Time
	MFARequired  bool
	MFAChallenge MFAChallengeToken
}

type LoginService struct {
	passwords PasswordStore
	sessions  CredentialSessionCreator
	throttler LoginThrottler
	mfa       MFAChallengeIssuer
	auditor   SecurityAuditor
	now       func() time.Time
	ttl       time.Duration
}

type LoginOption func(*LoginService) error

func WithSessionTTL(ttl time.Duration) LoginOption {
	return func(service *LoginService) error {
		if ttl < minSessionTTL || ttl > maxSessionTTL {
			return errors.New("session TTL must be between 5 minutes and 30 days")
		}
		service.ttl = ttl
		return nil
	}
}

func WithLoginClock(now func() time.Time) LoginOption {
	return func(service *LoginService) error {
		if now == nil {
			return errors.New("login clock is required")
		}
		service.now = now
		return nil
	}
}

func WithLoginThrottler(throttler LoginThrottler) LoginOption {
	return func(service *LoginService) error {
		if throttler == nil {
			return errors.New("login throttler is required")
		}
		service.throttler = throttler
		return nil
	}
}

func WithMFAChallengeIssuer(issuer MFAChallengeIssuer) LoginOption {
	return func(service *LoginService) error {
		if issuer == nil {
			return errors.New("MFA challenge issuer is required")
		}
		service.mfa = issuer
		return nil
	}
}

func WithSecurityAuditor(auditor SecurityAuditor) LoginOption {
	return func(service *LoginService) error {
		if auditor == nil {
			return errors.New("security auditor is required")
		}
		service.auditor = auditor
		return nil
	}
}

// NewLoginService builds the password sign-in use case. Without
// WithLoginThrottler it counts attempts in a MemoryLoginThrottler, per
// process; with FORGE_ENV=production it then logs a warning through
// slog.Default, because replicas and restarts each get a fresh budget. Pass
// auth/postgres.NewLoginThrottler to share the counters.
func NewLoginService(passwords PasswordStore, sessions CredentialSessionCreator, options ...LoginOption) (*LoginService, error) {
	if passwords == nil || sessions == nil {
		return nil, errors.New("password store and session creator are required")
	}
	throttler, err := NewMemoryLoginThrottler(DefaultLoginThrottleConfig())
	if err != nil {
		return nil, err
	}
	service := &LoginService{
		passwords: passwords,
		sessions:  sessions,
		throttler: throttler,
		now:       time.Now,
		ttl:       defaultSessionTTL,
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil login option")
		}
		if err := option(service); err != nil {
			return nil, err
		}
	}
	warnProcessLocalThrottle(nil, "sign-in", service.throttler)
	return service, nil
}

// Login verifies an active identity in an explicitly named tenant and creates
// an opaque session. All authentication misses return ErrCredentialsInvalid;
// infrastructure and corrupted persisted hashes remain server-side failures.
//
// A login or tenant that is not canonical (a non-ASCII address, a malformed
// slug) names no account, but it is not answered faster than one that does:
// it is throttled, the credential store is queried with random identifiers
// and a password hash is computed, so neither the response nor its timing
// tells a malformed organization from an unknown one or a wrong password.
func (s *LoginService) Login(ctx context.Context, input LoginInput) (LoginResult, error) {
	if input.Password == "" || len(input.Password) > maxPasswordBytes {
		return LoginResult{}, ErrCredentialsInvalid
	}
	email, emailOK := CanonicalLogin(input.Email)
	tenant, tenantOK := canonicalTenant(input.TenantSlug)
	wellFormed := emailOK && tenantOK
	account := email
	if !emailOK {
		account = invalidLoginAccount(input.Email)
	}

	throttleKey := deriveLoginThrottleKey(account, input.Source)
	decision, err := s.throttler.Attempt(ctx, throttleKey)
	if err != nil {
		return LoginResult{}, err
	}
	if !decision.Allowed {
		throttled := &LoginThrottledError{RetryAfter: decision.RetryAfter}
		auditErr := s.recordLoginEvent(ctx, SecurityLoginThrottled, SecurityOutcomeDenied, uuid.UUID{}, uuid.UUID{}, throttleKey)
		return LoginResult{}, errors.Join(throttled, auditErr)
	}

	lookupEmail, lookupTenant := email, tenant
	if !wellFormed {
		lookupEmail, lookupTenant = randomLookup()
	}
	identity, found, err := s.passwords.LookupPassword(ctx, lookupEmail, lookupTenant)
	if err != nil {
		return LoginResult{}, err
	}
	if !found || !wellFormed {
		_, _, verifyErr := VerifyPassword(dummyPasswordHash, input.Password)
		if verifyErr != nil {
			return LoginResult{}, verifyErr
		}
		auditErr := s.recordLoginEvent(ctx, SecurityLoginFailed, SecurityOutcomeDenied, uuid.UUID{}, uuid.UUID{}, throttleKey)
		return LoginResult{}, errors.Join(ErrCredentialsInvalid, auditErr)
	}
	if identity.SubjectID.Version() != 7 || identity.SubjectID.Variant() != 2 ||
		identity.MembershipID.Version() != 7 || identity.MembershipID.Variant() != 2 ||
		identity.CredentialVersion < 1 {
		return LoginResult{}, errors.New("password store returned an invalid identity")
	}

	match, needsRehash, err := VerifyPassword(identity.PasswordHash, input.Password)
	if err != nil {
		return LoginResult{}, err
	}
	if !match {
		auditErr := s.recordLoginEvent(ctx, SecurityLoginFailed, SecurityOutcomeDenied, identity.SubjectID, identity.MembershipID, throttleKey)
		return LoginResult{}, errors.Join(ErrCredentialsInvalid, auditErr)
	}
	if needsRehash {
		nextHash, err := HashPassword(input.Password)
		if err != nil {
			return LoginResult{}, err
		}
		replaced, err := s.passwords.ReplacePasswordHash(ctx, identity.SubjectID, identity.PasswordHash, nextHash)
		if err != nil {
			return LoginResult{}, err
		}
		if !replaced {
			// A concurrent password reset or credential update won the race.
			// Do not issue a session from the stale credential we verified.
			auditErr := s.recordLoginEvent(ctx, SecurityLoginFailed, SecurityOutcomeDenied, identity.SubjectID, identity.MembershipID, throttleKey)
			return LoginResult{}, errors.Join(ErrCredentialsInvalid, auditErr)
		}
	}

	if err := s.throttler.Success(ctx, throttleKey); err != nil {
		return LoginResult{}, err
	}
	expiresAt := s.now().UTC().Add(s.ttl)
	if identity.MFARequired {
		if s.mfa == nil {
			return LoginResult{}, errors.New("MFA is required but no challenge issuer is configured")
		}
		challenge, challengeExpiresAt, err := s.mfa.IssueMFAChallenge(ctx, identity.MembershipID, identity.CredentialVersion, expiresAt)
		if err != nil {
			return LoginResult{}, err
		}
		return LoginResult{
			MFARequired:  true,
			MFAChallenge: challenge,
			ExpiresAt:    challengeExpiresAt,
		}, nil
	}
	token, err := s.sessions.CreateSessionAtVersion(ctx, identity.MembershipID, identity.CredentialVersion, expiresAt)
	if err != nil {
		return LoginResult{}, err
	}
	if err := s.recordLoginEvent(ctx, SecurityLoginSucceeded, SecurityOutcomeSucceeded, identity.SubjectID, identity.MembershipID, throttleKey); err != nil {
		if revoker, ok := s.sessions.(SessionRevoker); ok {
			err = errors.Join(err, revoker.RevokeSession(ctx, token))
		}
		return LoginResult{}, err
	}
	return LoginResult{Token: token, ExpiresAt: expiresAt}, nil
}

func (s *LoginService) recordLoginEvent(
	ctx context.Context,
	kind SecurityEventKind,
	outcome SecurityOutcome,
	subjectID, membershipID uuid.UUID,
	key LoginThrottleKey,
) error {
	if s.auditor == nil {
		return nil
	}
	event := SecurityEvent{
		Kind:          kind,
		Outcome:       outcome,
		SubjectID:     subjectID,
		MembershipID:  membershipID,
		AccountDigest: Digest(key.Account),
	}
	if key.HasSource {
		event.SourceDigest = Digest(key.Source)
	}
	if err := s.auditor.RecordSecurityEvent(ctx, event); err != nil {
		return &SecurityAuditError{Cause: err}
	}
	return nil
}

// randomLookup returns a login and a tenant slug that no account has, for the
// credential lookup of a malformed sign-in.
func randomLookup() (email, tenant string) {
	return strings.ToLower(rand.Text()) + "@invalid.example", "z" + strings.ToLower(rand.Text())
}

// invalidLoginAccount is the account a login that is not canonical is
// counted under: its trimmed text, bounded. Such a login names no account, so
// no guess is possible, but the attempt still costs a password hash, and the
// source limits and a limit on the typed text stop it being a free
// computation.
func invalidLoginAccount(typed string) string {
	typed = strings.TrimSpace(typed)
	if len(typed) > 256 {
		typed = typed[:256]
	}
	return "invalid\x00" + typed
}

// MaxLoginBytes is the longest login (an email address) Forge accepts.
const MaxLoginBytes = 320

// CanonicalLogin returns the one canonical form of a login, the email
// address a person types: surrounding whitespace removed and ASCII letters
// lowercased. ok is false unless the login is 1 to MaxLoginBytes printable
// ASCII bytes (no space, control or non-ASCII character) with exactly one
// '@' and text on both sides of it.
//
// Storage (provisioning), lookup, throttle keys and recovery all use this
// form. Unicode case mapping differs between Go, PostgreSQL collations and
// mail systems (PostgreSQL can fold user@İcloud.com to user@icloud.com and
// strings.ToLower maps the Kelvin sign to 'k'), so a look-alike spelling
// could match another account, get its own throttle counters or receive
// another account's reset link. A non-ASCII login is therefore refused at
// the boundary and treated as an unknown account.
func CanonicalLogin(email string) (login string, ok bool) {
	email = strings.TrimSpace(email)
	if email == "" || len(email) > MaxLoginBytes {
		return "", false
	}
	for i := 0; i < len(email); i++ {
		if c := email[i]; c <= ' ' || c >= 0x7f {
			return "", false
		}
	}
	local, domain, found := strings.Cut(email, "@")
	if !found || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", false
	}
	return asciiLower(email), true
}

// NormalizeIdentity returns email and tenantSlug exactly as LoginService and
// RecoveryService look them up: the email in its CanonicalLogin form and the
// slug trimmed and ASCII-lowercased. ok is false when the email is not a
// canonical login or the slug does not match ^[a-z][a-z0-9-]{1,62}$.
// Provisioning code uses it so every stored identity can sign in.
func NormalizeIdentity(email, tenantSlug string) (normalizedEmail, normalizedTenant string, ok bool) {
	return normalizeLogin(email, tenantSlug)
}

func normalizeLogin(email, tenant string) (string, string, bool) {
	login, ok := CanonicalLogin(email)
	if !ok {
		return "", "", false
	}
	slug, ok := canonicalTenant(tenant)
	if !ok {
		return "", "", false
	}
	return login, slug, true
}

// canonicalTenant trims and ASCII-lowercases a tenant slug (never Unicode
// case mapping: U+212A KELVIN SIGN would otherwise become 'k') and checks it
// against the slug pattern.
func canonicalTenant(tenant string) (string, bool) {
	slug := asciiLower(strings.TrimSpace(tenant))
	return slug, tenantSlugPattern.MatchString(slug)
}

// asciiLower lowercases only the ASCII letters of s.
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			lower := []byte(s)
			for j := i; j < len(lower); j++ {
				if c := lower[j]; 'A' <= c && c <= 'Z' {
					lower[j] = c + 'a' - 'A'
				}
			}
			return string(lower)
		}
	}
	return s
}
