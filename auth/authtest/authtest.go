// Package authtest is the contract suite every Forge identity store must
// pass: auth/postgres, auth/sqlite and auth/mysql run it against a real
// database (decision D30). It drives the stores through the auth ports and
// the auth services, so the guarantees it checks (sign-in only through an
// active user, membership, tenant and organization; permissions only from
// the signed-in tenant's roles; one-time recovery tokens, MFA challenges,
// backup codes and TOTP periods; throttling that never exceeds its limits;
// setup that runs once) hold whichever database an application chooses.
//
//	func TestIdentityContract(t *testing.T) {
//	    authtest.Run(t, func(t testing.TB) authtest.Harness { ... a store on a fresh, migrated database ... })
//	}
package authtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// Store is everything an identity store implements.
type Store interface {
	auth.PasswordStore
	auth.SessionCreator
	auth.CredentialSessionCreator
	auth.SessionRevoker
	auth.Resolver
	auth.SecurityAuditor
	auth.RecoveryStore
	auth.RecoveryTokenChecker
	auth.MFAStore
	auth.MFALifecycleStore
	auth.MFASettingsStore
	auth.SubjectPasswordStore
	RevokeUserSessions(ctx context.Context, userID uuid.UUID) error
	PruneRecovery(ctx context.Context, limit int) (int64, error)
	PruneMFA(ctx context.Context, limit int) (int64, error)
}

// Throttler is a database login throttler.
type Throttler interface {
	auth.LoginThrottler
	PruneExpired(ctx context.Context, limit int) (int64, error)
}

// Owner is what Harness.Provision created.
type Owner struct {
	TenantID     uuid.UUID
	UserID       uuid.UUID
	MembershipID uuid.UUID
	RoleID       uuid.UUID
	Permissions  []string
}

// Harness is one store on a fresh, migrated database that the test cleans
// up, with what the suite needs to arrange its fixtures.
type Harness struct {
	Store Store
	// WithIdleTimeout returns a second store on the same database whose
	// sessions end after idle without use.
	WithIdleTimeout func(idle time.Duration) Store
	// NewThrottler returns a database throttler with config.
	NewThrottler func(config auth.LoginThrottleConfig) Throttler
	// Provision is the store's ProvisionOwner.
	Provision func(ctx context.Context, organization, slug, email, password string) (Owner, error)
	// ErrAlreadyProvisioned and ErrOwnerEmailTaken are the store's errors.
	ErrAlreadyProvisioned error
	ErrOwnerEmailTaken    error
	// Exec runs a fixture statement written with ? placeholders.
	Exec func(ctx context.Context, query string, args ...any) error
	// Count runs a query that returns one integer.
	Count func(ctx context.Context, query string, args ...any) (int64, error)
	// Time is the parameter a timestamp column takes for t.
	Time func(t time.Time) any
	// Close closes the database; every later call must fail.
	Close func()
}

// NewHarness returns a Harness on a fresh database.
type NewHarness func(t testing.TB) Harness

// Run runs the contract suite.
func Run(t *testing.T, newHarness NewHarness) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, *env)
	}{
		{"ProvisionOwnerOnce", provisionOwnerOnce},
		{"SignInResolvesTheTenantsPermissions", signInResolvesPermissions},
		{"RolesOfAnotherTenantGrantNothing", rolesOfAnotherTenantGrantNothing},
		{"InactiveChainSignsNobodyIn", inactiveChainSignsNobodyIn},
		{"SessionsAreRevocable", sessionsAreRevocable},
		{"CredentialVersionGuardsSessions", credentialVersionGuardsSessions},
		{"IdleSessionsEnd", idleSessionsEnd},
		{"PasswordHashCompareAndSet", passwordHashCompareAndSet},
		{"ThrottleCountsAndRefuses", throttleCountsAndRefuses},
		{"ThrottleHoldsUnderConcurrency", throttleHoldsUnderConcurrency},
		{"RecoveryTokensAreOneTime", recoveryTokensAreOneTime},
		{"SingleTenantIgnoresAPostedTenant", singleTenantIgnoresAPostedTenant},
		{"MFAEnrollmentAndChallenges", mfaEnrollmentAndChallenges},
		{"MFARotationDisableAndStepUp", mfaRotationDisableAndStepUp},
		{"MFAChallengeIsConsumedOnce", mfaChallengeIsConsumedOnce},
		{"SecurityEventsAreRecorded", securityEventsAreRecorded},
		{"ArgumentsAreValidated", argumentsAreValidated},
		{"ClosedDatabaseFails", closedDatabaseFails},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			test.run(t, &env{t: t, h: newHarness(t), ctx: context.Background()})
		})
	}
}

type env struct {
	t   *testing.T
	h   Harness
	ctx context.Context
}

func (e *env) exec(query string, args ...any) {
	e.t.Helper()
	err := e.h.Exec(e.ctx, query, args...)
	failIf(e.t, err != nil, "%s: %v", query, err)
}

func (e *env) count(query string, args ...any) int64 {
	e.t.Helper()
	n, err := e.h.Count(e.ctx, query, args...)
	failIf(e.t, err != nil, "%s: %v", query, err)
	return n
}

// fixtureHash is a password hash fixtures store; nothing verifies it.
const fixtureHash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g"

// tenant is an organization, a tenant and its slug.
type tenant struct {
	id   uuid.UUID
	org  uuid.UUID
	slug string
}

func (e *env) tenant(slug string) tenant {
	e.t.Helper()
	org, id := uuid.MustNew(), uuid.MustNew()
	e.exec(`INSERT INTO forge_organizations (id, name) VALUES (?, ?)`, org, slug+" Inc")
	e.exec(`INSERT INTO forge_tenants (id, organization_id, slug) VALUES (?, ?, ?)`, id, org, slug)
	return tenant{id: id, org: org, slug: slug}
}

func (e *env) user(email string) uuid.UUID {
	e.t.Helper()
	id := uuid.MustNew()
	e.exec(`INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES (?, ?, ?, ?)`, id, email, email, fixtureHash)
	return id
}

func (e *env) member(user uuid.UUID, in tenant) uuid.UUID {
	e.t.Helper()
	id := uuid.MustNew()
	e.exec(`INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES (?, ?, ?)`, id, user, in.id)
	return id
}

// grant gives membership a role in its tenant holding permissions.
func (e *env) grant(membership uuid.UUID, in tenant, role string, permissions ...string) uuid.UUID {
	e.t.Helper()
	id := uuid.MustNew()
	e.exec(`INSERT INTO forge_roles (id, tenant_id, name) VALUES (?, ?, ?)`, id, in.id, role)
	for _, permission := range permissions {
		if e.count(`SELECT COUNT(*) FROM forge_permissions WHERE name = ?`, permission) == 0 {
			e.exec(`INSERT INTO forge_permissions (name) VALUES (?)`, permission)
		}
		e.exec(`INSERT INTO forge_role_permissions (role_id, permission) VALUES (?, ?)`, id, permission)
	}
	e.exec(`INSERT INTO forge_membership_roles (membership_id, role_id, tenant_id) VALUES (?, ?, ?)`, membership, id, in.id)
	return id
}

// signedIn is a user with an active membership and a live session.
type signedIn struct {
	tenant     tenant
	user       uuid.UUID
	membership uuid.UUID
	token      auth.Token
}

func (e *env) signedIn(slug, email string, permissions ...string) signedIn {
	e.t.Helper()
	in := e.tenant(slug)
	user := e.user(email)
	membership := e.member(user, in)
	if len(permissions) > 0 {
		e.grant(membership, in, "member", permissions...)
	}
	token, err := e.h.Store.CreateSession(e.ctx, membership, time.Now().Add(time.Hour))
	must(e.t, err)
	return signedIn{tenant: in, user: user, membership: membership, token: token}
}

func (e *env) resolve(store Store, token auth.Token) (auth.Session, bool) {
	e.t.Helper()
	session, found, err := store.ResolveSession(e.ctx, token.Digest())
	must(e.t, err)
	return session, found
}

func digest(value string) auth.Digest { return sha256.Sum256([]byte(value)) }

func randomDigest(t testing.TB) auth.Digest {
	t.Helper()
	var d auth.Digest
	_, err := rand.Read(d[:])
	must(t, err)
	return d
}

func backupCodes(t testing.TB) []auth.Digest {
	t.Helper()
	codes := make([]auth.Digest, auth.DefaultMFABackupCodeCount)
	for i := range codes {
		codes[i] = randomDigest(t)
	}
	return codes
}

func must(t testing.TB, err error) {
	t.Helper()
	failIf(t, err != nil, "%v", err)
}

func provisionOwnerOnce(t *testing.T, e *env) {
	e.exec(`INSERT INTO forge_permissions (name) VALUES (?)`, "posts:read")
	e.exec(`INSERT INTO forge_permissions (name) VALUES (?)`, "posts:write")
	owner, err := e.h.Provision(e.ctx, "Acme Inc", "acme", "Owner@Example.com", "correct horse battery staple")
	must(t, err)
	failIf(t, len(owner.Permissions) != 2 || owner.Permissions[0] != "posts:read" || owner.Permissions[1] != "posts:write", "owner permissions = %v", owner.Permissions)
	got := e.count(`SELECT COUNT(*) FROM forge_security_audit_events WHERE kind = ? AND subject_id = ?`, string(auth.SecurityIdentityProvisioned), owner.UserID)
	failIf(t, got != 1, "provisioning events = %d", got)
	_, err = e.h.Provision(e.ctx, "Other", "other", "other@example.com", "correct horse battery staple")
	failIf(t, !errors.Is(err, e.h.ErrAlreadyProvisioned), "second setup: %v", err)
	got = e.count(`SELECT COUNT(*) FROM forge_tenants`)
	failIf(t, got != 1, "tenants after a refused setup = %d", got)

	// The owner signs in through the real login service, by tenant slug,
	// and holds every permission.
	login, err := auth.NewLoginService(e.h.Store, e.h.Store, auth.WithLoginThrottler(e.h.NewThrottler(auth.DefaultLoginThrottleConfig())), auth.WithSecurityAuditor(e.h.Store))
	must(t, err)
	_, err = login.Login(e.ctx, auth.LoginInput{Email: "owner@example.com", Password: "wrong password!!", TenantSlug: "acme", Source: "192.0.2.1"})
	failIf(t, !errors.Is(err, auth.ErrCredentialsInvalid), "wrong password: %v", err)
	result, err := login.Login(e.ctx, auth.LoginInput{Email: "owner@example.com", Password: "correct horse battery staple", TenantSlug: "acme", Source: "192.0.2.1"})
	must(t, err)
	session, found := e.resolve(e.h.Store, result.Token)
	failIf(t, !found || session.Principal.SubjectID() != owner.UserID || session.Principal.Tenant().ID != owner.TenantID || !session.Principal.Can("posts:write"), "owner session = %+v found=%v", session, found)
	// A single-tenant application signs in to the default tenant without
	// naming it; here no tenant has that slug, so nobody signs in.
	defaulted, err := auth.NewLoginService(e.h.Store, e.h.Store, auth.WithSingleTenant(auth.DefaultTenantSlug))
	must(t, err)
	_, err = defaulted.Login(e.ctx, auth.LoginInput{Email: "owner@example.com", Password: "correct horse battery staple", Source: "192.0.2.1"})
	failIf(t, !errors.Is(err, auth.ErrCredentialsInvalid), "sign-in to a missing default tenant: %v", err)
}

func signInResolvesPermissions(t *testing.T, e *env) {
	alice := e.signedIn("acme", "alice@example.com", "tasks:read", "tasks:write")
	e.grant(alice.membership, alice.tenant, "reader", "tasks:read", "notes:read")
	identity, found, err := e.h.Store.LookupPassword(e.ctx, "alice@example.com", "acme")
	failIf(t, err != nil || !found || identity.SubjectID != alice.user || identity.MembershipID != alice.membership || identity.CredentialVersion != 1 || identity.MFARequired || identity.PasswordHash != fixtureHash, "LookupPassword = %+v found=%v err=%v", identity, found, err)
	_, found, err = e.h.Store.LookupPassword(e.ctx, "alice@example.com", "other")
	failIf(t, err != nil || found, "LookupPassword in another tenant found=%v err=%v", found, err)
	session, found := e.resolve(e.h.Store, alice.token)
	failIf(t, !found || session.Principal.SubjectID() != alice.user || session.Principal.Tenant().ID != alice.tenant.id, "session = %+v found=%v", session, found)
	got := session.Principal.Permissions()
	failIf(t, len(got) != 3 || got[0] != "notes:read" || got[1] != "tasks:read" || got[2] != "tasks:write", "permissions = %v", got)
	failIf(t, session.ExpiresAt.Before(time.Now().Add(50*time.Minute)) || session.ExpiresAt.After(time.Now().Add(61*time.Minute)), "expiry = %v", session.ExpiresAt)
	// Permissions are read on every request, never from the token.
	e.exec(`DELETE FROM forge_role_permissions WHERE permission = ?`, "tasks:write")
	session, _ = e.resolve(e.h.Store, alice.token)
	failIf(t, session.Principal.Can("tasks:write"), "%v", "a removed permission is still granted")
	_, found = e.resolve(e.h.Store, auth.Token{})
	failIf(t, found, "%v", "an unknown token resolved")
	subject, found, err := e.h.Store.LookupSubjectPassword(e.ctx, alice.user)
	failIf(t, err != nil || !found || subject.Login != "alice@example.com" || subject.PasswordHash != fixtureHash, "LookupSubjectPassword = %+v %v %v", subject, found, err)
	_, found, err = e.h.Store.LookupSubjectPassword(e.ctx, uuid.MustNew())
	failIf(t, err != nil || found, "LookupSubjectPassword(unknown) found=%v err=%v", found, err)
}

// rolesOfAnotherTenantGrantNothing is the identity half of tenant
// isolation: one user in two tenants holds, in each, only that tenant's
// roles, and signs in to the tenant it names.
func rolesOfAnotherTenantGrantNothing(t *testing.T, e *env) {
	acme, globex := e.tenant("acme"), e.tenant("globex")
	user := e.user("bob@example.com")
	inAcme, inGlobex := e.member(user, acme), e.member(user, globex)
	e.grant(inGlobex, globex, "admin", "billing:write")
	e.grant(inAcme, acme, "reader", "billing:read")
	// A role of globex attached to the acme membership is refused by the
	// composite foreign key (membership, tenant) -> (role, tenant).
	globexRole := uuid.MustNew()
	e.exec(`INSERT INTO forge_roles (id, tenant_id, name) VALUES (?, ?, ?)`, globexRole, globex.id, "auditor")
	err := e.h.Exec(e.ctx, `INSERT INTO forge_membership_roles (membership_id, role_id, tenant_id) VALUES (?, ?, ?)`, inAcme, globexRole, acme.id)
	failIf(t, err == nil, "%v", "a role of another tenant was attached to a membership")
	identity, found, err := e.h.Store.LookupPassword(e.ctx, "bob@example.com", "acme")
	failIf(t, err != nil || !found || identity.MembershipID != inAcme, "acme sign-in found=%v membership=%s err=%v", found, identity.MembershipID, err)
	token, err := e.h.Store.CreateSession(e.ctx, inAcme, time.Now().Add(time.Hour))
	must(t, err)
	session, _ := e.resolve(e.h.Store, token)
	failIf(t, session.Principal.Tenant().ID != acme.id || session.Principal.Can("billing:write") || !session.Principal.Can("billing:read"), "acme session tenant=%s permissions=%v", session.Principal.Tenant().ID, session.Principal.Permissions())
	token, err = e.h.Store.CreateSession(e.ctx, inGlobex, time.Now().Add(time.Hour))
	must(t, err)
	session, _ = e.resolve(e.h.Store, token)
	failIf(t, session.Principal.Tenant().ID != globex.id || !session.Principal.Can("billing:write") || session.Principal.Can("billing:read"), "globex session tenant=%s permissions=%v", session.Principal.Tenant().ID, session.Principal.Permissions())
}

func inactiveChainSignsNobodyIn(t *testing.T, e *env) {
	for _, step := range []struct {
		name, disable, enable string
		target                func(signedIn) uuid.UUID
	}{
		{"user", `UPDATE forge_users SET status = 'disabled' WHERE id = ?`, `UPDATE forge_users SET status = 'active' WHERE id = ?`, func(s signedIn) uuid.UUID { return s.user }},
		{"membership", `UPDATE forge_memberships SET status = 'suspended' WHERE id = ?`, `UPDATE forge_memberships SET status = 'active' WHERE id = ?`, func(s signedIn) uuid.UUID { return s.membership }},
		{"tenant", `UPDATE forge_tenants SET status = 'disabled' WHERE id = ?`, `UPDATE forge_tenants SET status = 'active' WHERE id = ?`, func(s signedIn) uuid.UUID { return s.tenant.id }},
		{"organization", `UPDATE forge_organizations SET status = 'disabled' WHERE id = ?`, `UPDATE forge_organizations SET status = 'active' WHERE id = ?`, func(s signedIn) uuid.UUID { return s.tenant.org }},
	} {
		s := e.signedIn("t-"+step.name, step.name+"@example.com", "tasks:read")
		e.exec(step.disable, step.target(s))
		_, found := e.resolve(e.h.Store, s.token)
		failIf(t, found, "a session resolved with an inactive %s", step.name)
		_, found, err := e.h.Store.LookupPassword(e.ctx, step.name+"@example.com", "t-"+step.name)
		failIf(t, err != nil || found, "sign-in with an inactive %s found=%v err=%v", step.name, found, err)
		_, found, err = e.h.Store.LookupRecovery(e.ctx, step.name+"@example.com", "t-"+step.name)
		failIf(t, err != nil || found, "recovery with an inactive %s found=%v err=%v", step.name, found, err)
		_, err = e.h.Store.CreateSession(e.ctx, s.membership, time.Now().Add(time.Hour))
		failIf(t, err == nil, "a session was created with an inactive %s", step.name)
		e.exec(step.enable, step.target(s))
		_, found = e.resolve(e.h.Store, s.token)
		failIf(t, !found, "the session did not resolve again once the %s was active", step.name)
	}
}

func sessionsAreRevocable(t *testing.T, e *env) {
	s := e.signedIn("acme", "carol@example.com")
	other, err := e.h.Store.CreateSession(e.ctx, s.membership, time.Now().Add(time.Hour))
	must(t, err)
	must(t, e.h.Store.RevokeSession(e.ctx, s.token))
	must(t, e.h.Store.RevokeSession(e.ctx, s.token)) // revoking twice is not an error
	_, found := e.resolve(e.h.Store, s.token)
	failIf(t, found, "%v", "a revoked session resolved")
	_, found = e.resolve(e.h.Store, other)
	failIf(t, !found, "%v", "revoking one session revoked another")
	must(t, e.h.Store.RevokeUserSessions(e.ctx, s.user))
	_, found = e.resolve(e.h.Store, other)
	failIf(t, found, "%v", "RevokeUserSessions left a session")
	got := e.count(`SELECT session_version FROM forge_users WHERE id = ?`, s.user)
	failIf(t, got != 2, "session_version = %d", got)
	err = e.h.Store.RevokeUserSessions(e.ctx, uuid.MustNew())
	failIf(t, err == nil, "%v", "RevokeUserSessions of an unknown user succeeded")
	_, err = e.h.Store.CreateSession(e.ctx, uuid.MustNew(), time.Now().Add(time.Hour))
	failIf(t, err == nil, "%v", "a session was created for an unknown membership")
}

func credentialVersionGuardsSessions(t *testing.T, e *env) {
	s := e.signedIn("acme", "dave@example.com")
	token, err := e.h.Store.CreateSessionAtVersion(e.ctx, s.membership, 1, time.Now().Add(time.Hour))
	must(t, err)
	_, err = e.h.Store.CreateSessionAtVersion(e.ctx, s.membership, 2, time.Now().Add(time.Hour))
	failIf(t, !errors.Is(err, auth.ErrCredentialsInvalid), "a session at a future credential version: %v", err)
	e.exec(`UPDATE forge_users SET session_version = session_version + 1 WHERE id = ?`, s.user)
	_, found := e.resolve(e.h.Store, token)
	failIf(t, found, "%v", "a session of an earlier credential version resolved")
	_, err = e.h.Store.CreateSessionAtVersion(e.ctx, s.membership, 1, time.Now().Add(time.Hour))
	failIf(t, !errors.Is(err, auth.ErrCredentialsInvalid), "a session at a stale credential version: %v", err)
}

func idleSessionsEnd(t *testing.T, e *env) {
	s := e.signedIn("acme", "erin@example.com")
	idle := e.h.WithIdleTimeout(10 * time.Minute)
	// Used two minutes ago: still live, and the use is recorded.
	e.exec(`UPDATE forge_sessions SET last_seen_at = ? WHERE membership_id = ?`, e.h.Time(time.Now().Add(-2*time.Minute)), s.membership)
	_, found := e.resolve(idle, s.token)
	failIf(t, !found, "%v", "a session used two minutes ago ended")
	got := e.count(`SELECT COUNT(*) FROM forge_sessions WHERE membership_id = ? AND last_seen_at > ?`, s.membership, e.h.Time(time.Now().Add(-time.Minute)))
	failIf(t, got != 1, "%v", "resolving a stale session did not record its use")
	// Unused for longer than the idle timeout: it no longer resolves, and
	// the failed resolution does not revive it.
	e.exec(`UPDATE forge_sessions SET last_seen_at = ? WHERE membership_id = ?`, e.h.Time(time.Now().Add(-11*time.Minute)), s.membership)
	_, found = e.resolve(idle, s.token)
	failIf(t, found, "%v", "an idle session resolved")
	_, found = e.resolve(idle, s.token)
	failIf(t, found, "%v", "an idle session was revived")
	// The default store's idle timeout is two hours.
	_, found = e.resolve(e.h.Store, s.token)
	failIf(t, !found, "%v", "the two-hour default ended an 11-minute idle session")
}

func passwordHashCompareAndSet(t *testing.T, e *env) {
	s := e.signedIn("acme", "frank@example.com")
	replaced, err := e.h.Store.ReplacePasswordHash(e.ctx, s.user, fixtureHash, "next-hash")
	failIf(t, err != nil || !replaced, "ReplacePasswordHash = %v %v", replaced, err)
	replaced, err = e.h.Store.ReplacePasswordHash(e.ctx, s.user, fixtureHash, "other-hash")
	failIf(t, err != nil || replaced, "a stale compare-and-set replaced the hash: %v %v", replaced, err)
	e.exec(`UPDATE forge_users SET status = 'disabled' WHERE id = ?`, s.user)
	replaced, err = e.h.Store.ReplacePasswordHash(e.ctx, s.user, "next-hash", "third-hash")
	failIf(t, err != nil || replaced, "a disabled user's hash was replaced: %v %v", replaced, err)
}

func throttleKey(account, source string) auth.LoginThrottleKey {
	return auth.LoginThrottleKey{
		Account: sha256.Sum256([]byte(account)), Source: sha256.Sum256([]byte(source)), HasSource: true,
		Pair: sha256.Sum256([]byte(account + "|" + source)), HasPair: true,
	}
}

func throttleCountsAndRefuses(t *testing.T, e *env) {
	config := auth.DefaultLoginThrottleConfig()
	config.AccountLimit, config.SourceLimit, config.PairLimit, config.Window = 3, 10, 3, time.Minute
	throttler := e.h.NewThrottler(config)
	key := throttleKey("grace@example.com", "198.51.100.7")
	for i := range 3 {
		decision, err := throttler.Attempt(e.ctx, key)
		failIf(t, err != nil || !decision.Allowed, "attempt %d = %+v %v", i+1, decision, err)
	}
	decision, err := throttler.Attempt(e.ctx, key)
	failIf(t, err != nil || decision.Allowed || decision.RetryAfter <= 0 || decision.RetryAfter > time.Minute, "fourth attempt = %+v %v", decision, err)
	// A refused attempt counts nothing.
	got := e.count(`SELECT MAX(attempt_count) FROM forge_login_throttle`)
	failIf(t, got != 3, "counters after a refusal = %d", got)
	// Another source for the same account is still capped by the account.
	decision, err = throttler.Attempt(e.ctx, throttleKey("grace@example.com", "203.0.113.9"))
	failIf(t, err != nil || decision.Allowed, "another source = %+v %v", decision, err)
	// Success clears the account and pair counters, not the source's.
	must(t, throttler.Success(e.ctx, key))
	got = e.count(`SELECT COUNT(*) FROM forge_login_throttle WHERE scope = ?`, auth.SourceThrottleScope)
	failIf(t, got != 1, "source counters after success = %d", got)
	decision, err = throttler.Attempt(e.ctx, key)
	failIf(t, err != nil || !decision.Allowed, "attempt after success = %+v %v", decision, err)
	// Expired counters are pruned, and an expired counter starts over.
	e.exec(`UPDATE forge_login_throttle SET reset_at = ?`, e.h.Time(time.Now().Add(-time.Second)))
	pruned, err := throttler.PruneExpired(e.ctx, 100)
	failIf(t, err != nil || pruned == 0, "PruneExpired = %d %v", pruned, err)
	got = e.count(`SELECT COUNT(*) FROM forge_login_throttle`)
	failIf(t, got != 0, "counters after pruning = %d", got)
	_, err = throttler.PruneExpired(e.ctx, 0)
	failIf(t, err == nil, "%v", "PruneExpired(0) succeeded")
	for range 2 {
		_, err := throttler.Attempt(e.ctx, key)
		must(t, err)
	}
	e.exec(`UPDATE forge_login_throttle SET reset_at = ?`, e.h.Time(time.Now().Add(-time.Second)))
	decision, err = throttler.Attempt(e.ctx, key)
	failIf(t, err != nil || !decision.Allowed, "attempt after the window = %+v %v", decision, err)
	got = e.count(`SELECT MAX(attempt_count) FROM forge_login_throttle`)
	failIf(t, got != 1, "a counter past its window was not reset: %d", got)
}

func throttleHoldsUnderConcurrency(t *testing.T, e *env) {
	config := auth.DefaultLoginThrottleConfig()
	config.AccountLimit, config.SourceLimit, config.PairLimit, config.Window = 5, 50, 5, time.Minute
	throttler := e.h.NewThrottler(config)
	key := throttleKey("heidi@example.com", "198.51.100.8")
	const attempts = 16
	results := make(chan bool, attempts)
	errs := make(chan error, attempts)
	for range attempts {
		go func() {
			decision, err := throttler.Attempt(e.ctx, key)
			errs <- err
			results <- decision.Allowed
		}()
	}
	allowed := 0
	for range attempts {
		err := <-errs
		must(t, err)
		if <-results {
			allowed++
		}
	}
	failIf(t, allowed != 5, "%d of %d concurrent attempts were allowed, want exactly the limit 5", allowed, attempts)
}

func recoveryTokensAreOneTime(t *testing.T, e *env) {
	s := e.signedIn("acme", "ivan@example.com")
	identity, found, err := e.h.Store.LookupRecovery(e.ctx, "ivan@example.com", "acme")
	failIf(t, err != nil || !found || identity.SubjectID != s.user || identity.Email != "ivan@example.com", "LookupRecovery = %+v %v %v", identity, found, err)
	// A reset requested in another tenant finds nobody: ivan is no member
	// of globex, and other@ is no member of acme.
	e.signedIn("globex", "other@example.com")
	_, found, err = e.h.Store.LookupRecovery(e.ctx, "ivan@example.com", "globex")
	failIf(t, err != nil || found, "LookupRecovery in a tenant ivan is no member of found=%v err=%v", found, err)
	_, found, err = e.h.Store.LookupRecovery(e.ctx, "other@example.com", "acme")
	failIf(t, err != nil || found, "LookupRecovery of another tenant's user found=%v err=%v", found, err)
	first, second := digest("first"), digest("second")
	must(t, e.h.Store.CreateRecovery(e.ctx, s.user, first, time.Now().Add(time.Hour)))
	must(t, e.h.Store.CreateRecovery(e.ctx, s.user, second, time.Now().Add(time.Hour)))
	usable, err := e.h.Store.RecoveryTokenUsable(e.ctx, first)
	failIf(t, err != nil || usable, "a superseded token is usable: %v %v", usable, err)
	usable, err = e.h.Store.RecoveryTokenUsable(e.ctx, second)
	failIf(t, err != nil || !usable, "the newest token is not usable: %v %v", usable, err)
	consumed, err := e.h.Store.ConsumeRecovery(e.ctx, first, "reset-hash")
	failIf(t, err != nil || consumed, "a superseded token reset the password: %v %v", consumed, err)
	consumed, err = e.h.Store.ConsumeRecovery(e.ctx, second, "reset-hash")
	failIf(t, err != nil || !consumed, "ConsumeRecovery = %v %v", consumed, err)
	consumed, err = e.h.Store.ConsumeRecovery(e.ctx, second, "again-hash")
	failIf(t, err != nil || consumed, "a token was used twice: %v %v", consumed, err)
	_, found = e.resolve(e.h.Store, s.token)
	failIf(t, found, "%v", "a password reset left a session")
	got := e.count(`SELECT COUNT(*) FROM forge_users WHERE id = ? AND password_hash = ? AND session_version = 2`, s.user, "reset-hash")
	failIf(t, got != 1, "%v", "the reset did not replace the hash and bump the credential version")
	// Invalidate, expiry and unknown accounts.
	third := digest("third")
	must(t, e.h.Store.CreateRecovery(e.ctx, s.user, third, time.Now().Add(time.Hour)))
	must(t, e.h.Store.InvalidateRecovery(e.ctx, third))
	must(t, e.h.Store.InvalidateRecovery(e.ctx, digest("unknown")))
	consumed, err = e.h.Store.ConsumeRecovery(e.ctx, third, "x-hash")
	failIf(t, err != nil || consumed, "an invalidated token was used: %v %v", consumed, err)
	fourth := digest("fourth")
	must(t, e.h.Store.CreateRecovery(e.ctx, s.user, fourth, time.Now().Add(time.Hour)))
	e.exec(`UPDATE forge_password_recovery SET expires_at = ? WHERE consumed_at IS NULL`, e.h.Time(time.Now().Add(-time.Second)))
	consumed, err = e.h.Store.ConsumeRecovery(e.ctx, fourth, "x-hash")
	failIf(t, err != nil || consumed, "an expired token was used: %v %v", consumed, err)
	err = e.h.Store.CreateRecovery(e.ctx, uuid.MustNew(), digest("nobody"), time.Now().Add(time.Hour))
	failIf(t, err == nil, "%v", "a recovery was created for an unknown user")
	pruned, err := e.h.Store.PruneRecovery(e.ctx, 100)
	failIf(t, err != nil || pruned != 4, "PruneRecovery = %d %v", pruned, err)
	// Concurrent uses of one token: exactly one wins.
	fifth := digest("fifth")
	must(t, e.h.Store.CreateRecovery(e.ctx, s.user, fifth, time.Now().Add(time.Hour)))
	wins := make(chan bool, 8)
	for range 8 {
		go func() {
			consumed, err := e.h.Store.ConsumeRecovery(e.ctx, fifth, "race-hash")
			wins <- err == nil && consumed
		}()
	}
	won := 0
	for range 8 {
		if <-wins {
			won++
		}
	}
	failIf(t, won != 1, "%d concurrent resets succeeded, want 1", won)
}

// enroll gives s an active TOTP factor and returns its digest and backup
// codes.
func (e *env) enroll(s signedIn) (auth.Digest, []auth.Digest) {
	e.t.Helper()
	secret := randomDigest(e.t)
	must(e.t, e.h.Store.BeginTOTPEnrollment(e.ctx, s.user, secret, []byte("ciphertext-1"), time.Now().Add(10*time.Minute)))
	codes := backupCodes(e.t)
	confirmed, err := e.h.Store.ConfirmTOTPEnrollment(e.ctx, s.user, secret, 100, codes)
	failIf(e.t, err != nil || !confirmed, "ConfirmTOTPEnrollment = %v %v", confirmed, err)
	return secret, codes
}

func mfaEnrollmentAndChallenges(t *testing.T, e *env) {
	s := e.signedIn("acme", "judy@example.com")
	secret := randomDigest(t)
	must(t, e.h.Store.BeginTOTPEnrollment(e.ctx, s.user, randomDigest(t), []byte("ciphertext-0"), time.Now().Add(10*time.Minute)))
	must(t, e.h.Store.BeginTOTPEnrollment(e.ctx, s.user, secret, []byte("ciphertext-1"), time.Now().Add(10*time.Minute)))
	pending, found, err := e.h.Store.LoadPendingTOTP(e.ctx, s.user)
	failIf(t, err != nil || !found || pending.SecretDigest != secret || string(pending.SecretCiphertext) != "ciphertext-1" || pending.FactorActive, "LoadPendingTOTP = %+v %v %v", pending, found, err)
	confirmed, err := e.h.Store.ConfirmTOTPEnrollment(e.ctx, s.user, randomDigest(t), 100, backupCodes(t))
	failIf(t, err != nil || confirmed, "a different secret confirmed: %v %v", confirmed, err)
	codes := backupCodes(t)
	confirmed, err = e.h.Store.ConfirmTOTPEnrollment(e.ctx, s.user, secret, 100, codes)
	failIf(t, err != nil || !confirmed, "ConfirmTOTPEnrollment = %v %v", confirmed, err)
	_, found = e.resolve(e.h.Store, s.token)
	failIf(t, found, "%v", "enabling MFA left a session")
	err = e.h.Store.BeginTOTPEnrollment(e.ctx, s.user, randomDigest(t), []byte("x"), time.Now().Add(time.Minute))
	failIf(t, !errors.Is(err, auth.ErrMFAAlreadyEnabled), "a second enrollment: %v", err)
	status, err := e.h.Store.LoadMFAStatus(e.ctx, s.user)
	failIf(t, err != nil || !status.Enabled || status.BackupCodesRemaining != auth.DefaultMFABackupCodeCount, "LoadMFAStatus = %+v %v", status, err)
	identity, _, err := e.h.Store.LookupPassword(e.ctx, "judy@example.com", "acme")
	failIf(t, err != nil || !identity.MFARequired || identity.CredentialVersion != 2, "LookupPassword after MFA = %+v %v", identity, err)
	active, found, err := e.h.Store.LoadActiveTOTP(e.ctx, s.user)
	failIf(t, err != nil || !found || active.SecretDigest != secret || active.LastCounter != 100, "LoadActiveTOTP = %+v %v %v", active, found, err)

	// A login challenge: the code's period must be later than the last one.
	challenge := randomDigest(t)
	must(t, e.h.Store.CreateMFAChallenge(e.ctx, challenge, s.membership, 2, time.Now().Add(5*time.Minute), time.Now().Add(time.Hour)))
	record, found, err := e.h.Store.LoadMFAChallenge(e.ctx, challenge)
	failIf(t, err != nil || !found || record.MembershipID != s.membership || record.SubjectID != s.user || record.SecretDigest != secret || record.LastCounter != 100 || record.CredentialVersion != 2, "LoadMFAChallenge = %+v %v %v", record, found, err)
	_, consumed, err := e.h.Store.ConsumeMFAChallenge(e.ctx, challenge, secret, 100)
	failIf(t, err != nil || consumed, "a replayed period was accepted: %v %v", consumed, err)
	membership, consumed, err := e.h.Store.ConsumeMFAChallenge(e.ctx, challenge, secret, 101)
	failIf(t, err != nil || !consumed || membership != s.membership, "ConsumeMFAChallenge = %s %v %v", membership, consumed, err)
	_, consumed, err = e.h.Store.ConsumeMFAChallenge(e.ctx, challenge, secret, 102)
	failIf(t, err != nil || consumed, "a challenge was used twice: %v %v", consumed, err)
	_, found, _ = e.h.Store.LoadMFAChallenge(e.ctx, challenge)
	failIf(t, found, "%v", "a consumed challenge loaded")
	// A challenge at a stale credential version is refused.
	err = e.h.Store.CreateMFAChallenge(e.ctx, randomDigest(t), s.membership, 1, time.Now().Add(time.Minute), time.Now().Add(time.Hour))
	failIf(t, !errors.Is(err, auth.ErrCredentialsInvalid), "a stale challenge: %v", err)
	// A new challenge supersedes the previous one; a backup code completes it.
	old, backup := randomDigest(t), randomDigest(t)
	must(t, e.h.Store.CreateMFAChallenge(e.ctx, old, s.membership, 2, time.Now().Add(5*time.Minute), time.Now().Add(time.Hour)))
	must(t, e.h.Store.CreateMFAChallenge(e.ctx, backup, s.membership, 2, time.Now().Add(5*time.Minute), time.Now().Add(time.Hour)))
	_, found, _ = e.h.Store.LoadMFAChallenge(e.ctx, old)
	failIf(t, found, "%v", "a superseded challenge loaded")
	_, consumed, err = e.h.Store.ConsumeMFABackupChallenge(e.ctx, backup, randomDigest(t))
	failIf(t, err != nil || consumed, "an unknown backup code completed a challenge: %v %v", consumed, err)
	membership, consumed, err = e.h.Store.ConsumeMFABackupChallenge(e.ctx, backup, codes[0])
	failIf(t, err != nil || !consumed || membership != s.membership, "ConsumeMFABackupChallenge = %s %v %v", membership, consumed, err)
	again := randomDigest(t)
	must(t, e.h.Store.CreateMFAChallenge(e.ctx, again, s.membership, 2, time.Now().Add(5*time.Minute), time.Now().Add(time.Hour)))
	_, consumed, err = e.h.Store.ConsumeMFABackupChallenge(e.ctx, again, codes[0])
	failIf(t, err != nil || consumed, "a backup code was used twice: %v %v", consumed, err)

	// Settings: the counter only moves forward, backup codes are one-time.
	advanced, err := e.h.Store.AdvanceTOTPCounter(e.ctx, s.user, secret, 101)
	failIf(t, err != nil || advanced, "the counter moved back: %v %v", advanced, err)
	advanced, err = e.h.Store.AdvanceTOTPCounter(e.ctx, s.user, secret, 150)
	failIf(t, err != nil || !advanced, "AdvanceTOTPCounter = %v %v", advanced, err)
	has, err := e.h.Store.HasMFABackupCode(e.ctx, s.user, codes[1])
	failIf(t, err != nil || !has, "HasMFABackupCode = %v %v", has, err)
	spent, err := e.h.Store.ConsumeMFABackupCode(e.ctx, s.user, codes[1])
	failIf(t, err != nil || !spent, "ConsumeMFABackupCode = %v %v", spent, err)
	spent, err = e.h.Store.ConsumeMFABackupCode(e.ctx, s.user, codes[1])
	failIf(t, err != nil || spent, "a backup code was spent twice: %v %v", spent, err)
	has, err = e.h.Store.HasMFABackupCode(e.ctx, s.user, codes[1])
	failIf(t, err != nil || has, "a spent code is still there: %v %v", has, err)
	status, _ = e.h.Store.LoadMFAStatus(e.ctx, s.user)
	failIf(t, status.BackupCodesRemaining != auth.DefaultMFABackupCodeCount-2, "remaining codes = %d", status.BackupCodesRemaining)
	// Pruning removes consumed and expired challenges and expired enrollments.
	pruned, err := e.h.Store.PruneMFA(e.ctx, 100)
	failIf(t, err != nil || pruned < 3, "PruneMFA = %d %v", pruned, err)
}

func mfaRotationDisableAndStepUp(t *testing.T, e *env) {
	s := e.signedIn("acme", "mallory@example.com")
	err := e.h.Store.BeginTOTPRotation(e.ctx, s.user, randomDigest(t), []byte("c"), time.Now().Add(time.Minute), nil)
	failIf(t, !errors.Is(err, auth.ErrMFANotEnabled), "rotation without a factor: %v", err)
	disabled, err := e.h.Store.DisableMFA(e.ctx, s.user, nil)
	failIf(t, err != nil || disabled, "DisableMFA without a factor = %v %v", disabled, err)
	secret, codes := e.enroll(s)
	// A wrong step-up code changes nothing.
	bad := randomDigest(t)
	err = e.h.Store.BeginTOTPRotation(e.ctx, s.user, randomDigest(t), []byte("c"), time.Now().Add(time.Minute), &bad)
	failIf(t, !errors.Is(err, auth.ErrMFAStepUpInvalid), "rotation with a wrong step-up: %v", err)
	next := randomDigest(t)
	must(t, e.h.Store.BeginTOTPRotation(e.ctx, s.user, next, []byte("ciphertext-2"), time.Now().Add(10*time.Minute), &codes[0]))
	pending, found, err := e.h.Store.LoadPendingTOTP(e.ctx, s.user)
	failIf(t, err != nil || !found || pending.SecretDigest != next || !pending.FactorActive, "pending rotation = %+v %v %v", pending, found, err)
	confirmed, err := e.h.Store.ConfirmTOTPEnrollment(e.ctx, s.user, next, 1, backupCodes(t))
	failIf(t, !errors.Is(err, auth.ErrMFAAlreadyEnabled) || confirmed, "a rotation key confirmed as an enrollment: %v %v", confirmed, err)
	newCodes := backupCodes(t)
	confirmed, err = e.h.Store.ConfirmTOTPRotation(e.ctx, s.user, next, 200, newCodes, &codes[0])
	failIf(t, !errors.Is(err, auth.ErrMFAStepUpInvalid) || confirmed, "a spent step-up code confirmed a rotation: %v %v", confirmed, err)
	confirmed, err = e.h.Store.ConfirmTOTPRotation(e.ctx, s.user, randomDigest(t), 200, newCodes, nil)
	failIf(t, err != nil || confirmed, "an unknown rotation key confirmed: %v %v", confirmed, err)
	confirmed, err = e.h.Store.ConfirmTOTPRotation(e.ctx, s.user, next, 200, newCodes, &codes[1])
	failIf(t, err != nil || !confirmed, "ConfirmTOTPRotation = %v %v", confirmed, err)
	active, _, _ := e.h.Store.LoadActiveTOTP(e.ctx, s.user)
	failIf(t, active.SecretDigest != next || active.SecretDigest == secret || active.LastCounter != 200, "active factor after rotation = %+v", active)
	has, _ := e.h.Store.HasMFABackupCode(e.ctx, s.user, codes[2])
	failIf(t, has, "%v", "a backup code of the old set survived the rotation")
	// Regenerating backup codes, then disabling.
	replaced, err := e.h.Store.ReplaceMFABackupCodes(e.ctx, s.user, backupCodes(t), &bad)
	failIf(t, !errors.Is(err, auth.ErrMFAStepUpInvalid) || replaced, "regeneration with a wrong step-up: %v %v", replaced, err)
	replaced, err = e.h.Store.ReplaceMFABackupCodes(e.ctx, s.user, backupCodes(t), &newCodes[0])
	failIf(t, err != nil || !replaced, "ReplaceMFABackupCodes = %v %v", replaced, err)
	disabled, err = e.h.Store.DisableMFA(e.ctx, s.user, &newCodes[1])
	failIf(t, !errors.Is(err, auth.ErrMFAStepUpInvalid) || disabled, "disable with a replaced code: %v %v", disabled, err)
	disabled, err = e.h.Store.DisableMFA(e.ctx, s.user, nil)
	failIf(t, err != nil || !disabled, "DisableMFA = %v %v", disabled, err)
	status, _ := e.h.Store.LoadMFAStatus(e.ctx, s.user)
	failIf(t, status.Enabled || status.BackupCodesRemaining != 0, "status after disable = %+v", status)
	replaced, err = e.h.Store.ReplaceMFABackupCodes(e.ctx, s.user, backupCodes(t), nil)
	failIf(t, err != nil || replaced, "codes regenerated without a factor: %v %v", replaced, err)
	_, found, err = e.h.Store.LoadActiveTOTP(e.ctx, s.user)
	failIf(t, err != nil || found, "a factor after disable: %v %v", found, err)
	_, found, err = e.h.Store.LoadPendingTOTP(e.ctx, s.user)
	failIf(t, err != nil || found, "a pending key after disable: %v %v", found, err)
	err = e.h.Store.CreateMFAChallenge(e.ctx, randomDigest(t), s.membership, 4, time.Now().Add(time.Minute), time.Now().Add(time.Hour))
	failIf(t, err == nil, "%v", "a challenge was created without a factor")
}

func mfaChallengeIsConsumedOnce(t *testing.T, e *env) {
	s := e.signedIn("acme", "niaj@example.com")
	secret, codes := e.enroll(s)
	challenge := randomDigest(t)
	must(t, e.h.Store.CreateMFAChallenge(e.ctx, challenge, s.membership, 2, time.Now().Add(5*time.Minute), time.Now().Add(time.Hour)))
	const racers = 8
	wins := make(chan bool, racers)
	for i := range racers {
		go func() {
			var consumed bool
			var err error
			if i%2 == 0 {
				_, consumed, err = e.h.Store.ConsumeMFAChallenge(e.ctx, challenge, secret, 300)
			} else {
				_, consumed, err = e.h.Store.ConsumeMFABackupChallenge(e.ctx, challenge, codes[i])
			}
			wins <- err == nil && consumed
		}()
	}
	won := 0
	for range racers {
		if <-wins {
			won++
		}
	}
	failIf(t, won != 1, "%d concurrent completions of one challenge succeeded, want 1", won)
}

func securityEventsAreRecorded(t *testing.T, e *env) {
	actor := uuid.MustNew()
	must(t, e.h.Store.RecordSecurityEvent(e.ctx, auth.SecurityEvent{
		Kind: auth.SecurityLoginSucceeded, Outcome: auth.SecurityOutcomeSucceeded, ActorID: actor, SubjectID: actor,
		AccountDigest: digest("a"), SourceDigest: digest("s"), CredentialDigest: digest("c"),
	}))
	must(t, e.h.Store.RecordSecurityEvent(e.ctx, auth.SecurityEvent{
		Kind: auth.SecurityLoginSucceeded, Outcome: auth.SecurityOutcomeDenied, OccurredAt: time.Now().Add(-time.Hour),
	}))
	got := e.count(`SELECT COUNT(*) FROM forge_security_audit_events WHERE kind = ?`, string(auth.SecurityLoginSucceeded))
	failIf(t, got != 2, "events = %d", got)
	got = e.count(`SELECT COUNT(*) FROM forge_security_audit_events WHERE actor_id = ? AND account_digest IS NOT NULL AND request_id IS NULL`, actor)
	failIf(t, got != 1, "the event's columns were not stored: %d", got)
	err := e.h.Store.RecordSecurityEvent(e.ctx, auth.SecurityEvent{Kind: "BAD KIND", Outcome: auth.SecurityOutcomeDenied})
	failIf(t, err == nil, "%v", "an invalid event was recorded")
}

func argumentsAreValidated(t *testing.T, e *env) {
	store, zero, user := e.h.Store, uuid.UUID{}, uuid.MustNew()
	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	codes := backupCodes(t)
	checks := map[string]error{}
	_, checks["ReplacePasswordHash bad id"] = store.ReplacePasswordHash(e.ctx, zero, "a", "b")
	_, checks["ReplacePasswordHash same hash"] = store.ReplacePasswordHash(e.ctx, user, "a", "a")
	_, checks["CreateSession bad id"] = store.CreateSession(e.ctx, zero, future)
	_, checks["CreateSession past"] = store.CreateSession(e.ctx, user, past)
	_, checks["CreateSessionAtVersion zero"] = store.CreateSessionAtVersion(e.ctx, user, 0, future)
	checks["RevokeUserSessions bad id"] = store.RevokeUserSessions(e.ctx, zero)
	checks["CreateRecovery bad id"] = store.CreateRecovery(e.ctx, zero, digest("x"), future)
	checks["CreateRecovery past"] = store.CreateRecovery(e.ctx, user, digest("x"), past)
	_, checks["ConsumeRecovery empty hash"] = store.ConsumeRecovery(e.ctx, digest("x"), "")
	_, checks["PruneRecovery 0"] = store.PruneRecovery(e.ctx, 0)
	_, checks["PruneMFA 0"] = store.PruneMFA(e.ctx, 0)
	checks["BeginTOTPEnrollment bad id"] = store.BeginTOTPEnrollment(e.ctx, zero, digest("x"), []byte("c"), future)
	checks["BeginTOTPEnrollment no secret"] = store.BeginTOTPEnrollment(e.ctx, user, digest("x"), nil, future)
	checks["BeginTOTPEnrollment past"] = store.BeginTOTPEnrollment(e.ctx, user, digest("x"), []byte("c"), past)
	checks["BeginTOTPEnrollment unknown user"] = store.BeginTOTPEnrollment(e.ctx, user, digest("x"), []byte("c"), future)
	_, checks["ConfirmTOTPEnrollment negative"] = store.ConfirmTOTPEnrollment(e.ctx, user, digest("x"), -1, codes)
	_, checks["ConfirmTOTPEnrollment codes"] = store.ConfirmTOTPEnrollment(e.ctx, user, digest("x"), 1, codes[:1])
	checks["CreateMFAChallenge bad id"] = store.CreateMFAChallenge(e.ctx, digest("x"), zero, 1, future, future)
	checks["CreateMFAChallenge version"] = store.CreateMFAChallenge(e.ctx, digest("x"), user, 0, future, future)
	checks["CreateMFAChallenge past"] = store.CreateMFAChallenge(e.ctx, digest("x"), user, 1, past, future)
	checks["CreateMFAChallenge unknown"] = store.CreateMFAChallenge(e.ctx, digest("x"), user, 1, future, future)
	_, _, checks["ConsumeMFAChallenge negative"] = store.ConsumeMFAChallenge(e.ctx, digest("x"), digest("y"), -1)
	checks["BeginTOTPRotation bad id"] = store.BeginTOTPRotation(e.ctx, zero, digest("x"), []byte("c"), future, nil)
	checks["BeginTOTPRotation no secret"] = store.BeginTOTPRotation(e.ctx, user, digest("x"), nil, future, nil)
	checks["BeginTOTPRotation past"] = store.BeginTOTPRotation(e.ctx, user, digest("x"), []byte("c"), past, nil)
	_, checks["ConfirmTOTPRotation negative"] = store.ConfirmTOTPRotation(e.ctx, user, digest("x"), -1, codes, nil)
	_, checks["ConfirmTOTPRotation codes"] = store.ConfirmTOTPRotation(e.ctx, user, digest("x"), 1, nil, nil)
	_, checks["DisableMFA bad id"] = store.DisableMFA(e.ctx, zero, nil)
	_, _, checks["LookupSubjectPassword bad id"] = store.LookupSubjectPassword(e.ctx, zero)
	_, checks["AdvanceTOTPCounter negative"] = store.AdvanceTOTPCounter(e.ctx, user, digest("x"), -1)
	_, checks["ReplaceMFABackupCodes bad id"] = store.ReplaceMFABackupCodes(e.ctx, zero, codes, nil)
	_, checks["ReplaceMFABackupCodes codes"] = store.ReplaceMFABackupCodes(e.ctx, user, codes[:2], nil)
	_, checks["Provision bad slug"] = e.h.Provision(e.ctx, "Acme", "A B", "owner@example.com", "correct horse battery staple")
	_, checks["Provision bad name"] = e.h.Provision(e.ctx, "bad\x07name", "acme", "owner@example.com", "correct horse battery staple")
	_, checks["Provision empty password"] = e.h.Provision(e.ctx, "Acme", "acme", "owner@example.com", "")
	for name, err := range checks {
		errorIf(t, err == nil, "%s: accepted", name)
	}
	// Nothing was written by the refused calls.
	got := e.count(`SELECT COUNT(*) FROM forge_tenants`)
	failIf(t, got != 0, "tenants = %d", got)
	// An owner whose email is taken.
	e.user("taken@example.com")
	_, err := e.h.Provision(e.ctx, "Acme", "acme", "taken@example.com", "correct horse battery staple")
	failIf(t, !errors.Is(err, e.h.ErrOwnerEmailTaken), "taken email: %v", err)
}

func closedDatabaseFails(t *testing.T, e *env) {
	s := e.signedIn("acme", "olivia@example.com")
	throttler := e.h.NewThrottler(auth.DefaultLoginThrottleConfig())
	e.h.Close()
	store, future, user, d := e.h.Store, time.Now().Add(time.Hour), s.user, digest("x")
	codes := backupCodes(t)
	failures := map[string]error{}
	_, _, failures["LookupPassword"] = store.LookupPassword(e.ctx, "a@example.com", "acme")
	_, failures["ReplacePasswordHash"] = store.ReplacePasswordHash(e.ctx, user, "a", "b")
	_, failures["CreateSession"] = store.CreateSession(e.ctx, s.membership, future)
	failures["RevokeSession"] = store.RevokeSession(e.ctx, s.token)
	failures["RevokeUserSessions"] = store.RevokeUserSessions(e.ctx, user)
	_, _, failures["ResolveSession"] = store.ResolveSession(e.ctx, s.token.Digest())
	failures["RecordSecurityEvent"] = store.RecordSecurityEvent(e.ctx, auth.SecurityEvent{Kind: auth.SecurityLoginSucceeded, Outcome: auth.SecurityOutcomeSucceeded})
	_, _, failures["LookupRecovery"] = store.LookupRecovery(e.ctx, "a@example.com", "acme")
	_, failures["RecoveryTokenUsable"] = store.RecoveryTokenUsable(e.ctx, d)
	failures["CreateRecovery"] = store.CreateRecovery(e.ctx, user, d, future)
	failures["InvalidateRecovery"] = store.InvalidateRecovery(e.ctx, d)
	_, failures["ConsumeRecovery"] = store.ConsumeRecovery(e.ctx, d, "h")
	_, failures["PruneRecovery"] = store.PruneRecovery(e.ctx, 10)
	failures["BeginTOTPEnrollment"] = store.BeginTOTPEnrollment(e.ctx, user, d, []byte("c"), future)
	_, _, failures["LoadPendingTOTP"] = store.LoadPendingTOTP(e.ctx, user)
	_, failures["ConfirmTOTPEnrollment"] = store.ConfirmTOTPEnrollment(e.ctx, user, d, 1, codes)
	failures["CreateMFAChallenge"] = store.CreateMFAChallenge(e.ctx, d, s.membership, 1, future, future)
	_, _, failures["LoadMFAChallenge"] = store.LoadMFAChallenge(e.ctx, d)
	_, _, failures["ConsumeMFAChallenge"] = store.ConsumeMFAChallenge(e.ctx, d, d, 1)
	_, _, failures["ConsumeMFABackupChallenge"] = store.ConsumeMFABackupChallenge(e.ctx, d, d)
	failures["BeginTOTPRotation"] = store.BeginTOTPRotation(e.ctx, user, d, []byte("c"), future, nil)
	_, failures["ConfirmTOTPRotation"] = store.ConfirmTOTPRotation(e.ctx, user, d, 1, codes, nil)
	_, failures["DisableMFA"] = store.DisableMFA(e.ctx, user, nil)
	_, failures["PruneMFA"] = store.PruneMFA(e.ctx, 10)
	_, _, failures["LookupSubjectPassword"] = store.LookupSubjectPassword(e.ctx, user)
	_, failures["LoadMFAStatus"] = store.LoadMFAStatus(e.ctx, user)
	_, _, failures["LoadActiveTOTP"] = store.LoadActiveTOTP(e.ctx, user)
	_, failures["AdvanceTOTPCounter"] = store.AdvanceTOTPCounter(e.ctx, user, d, 1)
	_, failures["ConsumeMFABackupCode"] = store.ConsumeMFABackupCode(e.ctx, user, d)
	_, failures["HasMFABackupCode"] = store.HasMFABackupCode(e.ctx, user, d)
	_, failures["ReplaceMFABackupCodes"] = store.ReplaceMFABackupCodes(e.ctx, user, codes, nil)
	_, failures["Attempt"] = throttler.Attempt(e.ctx, throttleKey("a", "b"))
	failures["Success"] = throttler.Success(e.ctx, throttleKey("a", "b"))
	_, failures["PruneExpired"] = throttler.PruneExpired(e.ctx, 10)
	_, failures["Provision"] = e.h.Provision(e.ctx, "Acme", "acme2", "p@example.com", "correct horse battery staple")
	for name, err := range failures {
		errorIf(t, err == nil, "%s succeeded on a closed database", name)
	}
}

// failIf stops the test with the message when failed is true.
func failIf(t testing.TB, failed bool, format string, args ...any) {
	t.Helper()
	if failed {
		t.Fatalf(format, args...)
	}
}

// errorIf reports the message, and lets the test go on, when failed is true.
func errorIf(t testing.TB, failed bool, format string, args ...any) {
	t.Helper()
	if failed {
		t.Errorf(format, args...)
	}
}

// singleTenantIgnoresAPostedTenant: a single-organization application's
// login service (auth.WithSingleTenant) signs in to its one tenant whatever
// tenant the client posts, so posting another organization's slug never
// signs in to that organization.
func singleTenantIgnoresAPostedTenant(t *testing.T, e *env) {
	const password = "correct horse battery staple"
	hash, err := auth.DefaultPasswordHasher().Hash(e.ctx, password)
	must(t, err)
	acme, globex := e.tenant("acme"), e.tenant("globex")
	both, outsider := e.user("both@example.com"), e.user("outsider@example.com")
	e.exec(`UPDATE forge_users SET password_hash = ? WHERE id IN (?, ?)`, hash, both, outsider)
	e.member(both, acme)
	e.member(both, globex)
	e.member(outsider, globex)
	login, err := auth.NewLoginService(e.h.Store, e.h.Store, auth.WithSingleTenant("acme"))
	must(t, err)
	_, err = login.Login(e.ctx, auth.LoginInput{Email: "outsider@example.com", Password: password, TenantSlug: "globex", Source: "192.0.2.1"})
	failIf(t, !errors.Is(err, auth.ErrCredentialsInvalid), "a member of globex only signed in to a single-tenant application by posting globex: %v", err)
	result, err := login.Login(e.ctx, auth.LoginInput{Email: "both@example.com", Password: password, TenantSlug: "globex", Source: "192.0.2.1"})
	must(t, err)
	session, found := e.resolve(e.h.Store, result.Token)
	failIf(t, !found, "the session of a single-tenant sign-in does not resolve")
	failIf(t, session.Principal.Tenant().ID != acme.id, "posting globex signed in to tenant %v, want acme %v", session.Principal.Tenant().ID, acme.id)
}
