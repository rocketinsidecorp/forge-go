package sqlidentity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var (
	// ErrMembershipRequired is returned when the membership a session is
	// created for is missing or not active.
	ErrMembershipRequired = errors.New("active membership is required")
	// ErrExpiryRequired is returned when the expiry of a new session is not
	// in the future.
	ErrExpiryRequired = errors.New("session expiry must be in the future")

	_ auth.SessionCreator           = (*Repository)(nil)
	_ auth.CredentialSessionCreator = (*Repository)(nil)
	_ auth.PasswordStore            = (*Repository)(nil)
	_ auth.SessionRevoker           = (*Repository)(nil)
	_ auth.Resolver                 = (*Repository)(nil)
)

// Session idle timeout bounds and the default, as in auth/postgres.
const (
	DefaultSessionIdleTimeout = 2 * time.Hour
	minSessionIdleTimeout     = 5 * time.Minute
	maxSessionIdleTimeout     = 30 * 24 * time.Hour
	// sessionTouchInterval is how stale last_seen_at may get before a
	// resolution records the use: at most one write per session and
	// interval, however many requests the session makes.
	sessionTouchInterval = time.Minute
)

// Repository is the identity store: password identities, sessions, password
// recovery, MFA factors and challenges, and the security audit trail.
type Repository struct {
	db          *sqldb.DB
	dialect     *Dialect
	now         func() time.Time
	idleTimeout time.Duration
	queries     sync.Map // query text -> text with the dialect's placeholders expanded
	replacer    *strings.Replacer
}

// Option configures a Repository.
type Option func(*Repository) error

// WithSessionIdleTimeout ends a session nobody used for timeout, before its
// absolute expiry: ResolveSession no longer finds it (fail closed), and
// signing in again creates a new one. timeout must be between five minutes
// and 30 days; New uses DefaultSessionIdleTimeout (two hours) without this
// option. Pass config.Auth.SessionIdleTimeout (FORGE_AUTH_SESSION_IDLE_TIMEOUT).
func WithSessionIdleTimeout(timeout time.Duration) Option {
	return func(r *Repository) error {
		if timeout < minSessionIdleTimeout || timeout > maxSessionIdleTimeout {
			return errors.New("session idle timeout must be between 5 minutes and 30 days")
		}
		r.idleTimeout = timeout
		return nil
	}
}

// WithClock sets the clock that checks the expiries callers pass (they must
// be in the future); the database clock decides everything else. It is for
// tests.
func WithClock(now func() time.Time) Option {
	return func(r *Repository) error {
		if now == nil {
			return errors.New("identity store clock is required")
		}
		r.now = now
		return nil
	}
}

// New returns the identity repository on db for dialect.
func New(db *sqldb.DB, dialect *Dialect, options ...Option) (*Repository, error) {
	if db == nil {
		return nil, errors.New("authentication database is required")
	}
	if err := dialect.validate(); err != nil {
		return nil, err
	}
	repository := &Repository{
		db: db, dialect: dialect, now: time.Now, idleTimeout: DefaultSessionIdleTimeout,
		replacer: strings.NewReplacer("{now}", dialect.Now, "{ago}", dialect.Ago, "{lock}", dialect.Lock, "{skip_locked}", dialect.SkipLocked),
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil repository option")
		}
		if err := option(repository); err != nil {
			return nil, err
		}
	}
	return repository, nil
}

// SessionIdleTimeout reports how long a session may go unused.
func (r *Repository) SessionIdleTimeout() time.Duration { return r.idleTimeout }

// q expands the dialect's placeholders in query.
func (r *Repository) q(query string) string {
	if cached, ok := r.queries.Load(query); ok {
		return cached.(string)
	}
	expanded := r.replacer.Replace(query)
	r.queries.Store(query, expanded)
	return expanded
}

// translate maps a database error to a fault (sqldb.Translate).
func (r *Repository) translate(err error) error { return r.db.Translate(err) }

// inTx runs fn in a read-write transaction with the dialect's options.
func (r *Repository) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return r.db.InTxWith(ctx, r.dialect.Tx, fn)
}

// activeChain is the condition every sign-in path checks: the user, the
// membership, the tenant and the organization are all active.
const activeChain = `
		  AND u.status = 'active'
		  AND m.status = 'active'
		  AND t.status = 'active'
		  AND o.status = 'active'`

// LookupPassword returns the active account with the normalized email and
// its active membership in the tenant with slug tenantSlug, with its password
// hash and whether MFA is enabled. found is false otherwise.
func (r *Repository) LookupPassword(ctx context.Context, emailNormalized, tenantSlug string) (auth.PasswordIdentity, bool, error) {
	var identity auth.PasswordIdentity
	err := r.db.QueryRowContext(ctx, r.q(`
		SELECT u.id, m.id, u.password_hash, u.session_version,
		       EXISTS (SELECT 1 FROM forge_totp_factors f WHERE f.user_id = u.id)
		FROM forge_users u
		JOIN forge_memberships m ON m.user_id = u.id
		JOIN forge_tenants t ON t.id = m.tenant_id
		JOIN forge_organizations o ON o.id = t.organization_id
		WHERE u.email_normalized = ?
		  AND t.slug = ?`+activeChain), emailNormalized, tenantSlug).
		Scan(&identity.SubjectID, &identity.MembershipID, &identity.PasswordHash, &identity.CredentialVersion, &identity.MFARequired)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.PasswordIdentity{}, false, nil
	}
	if err != nil {
		return auth.PasswordIdentity{}, false, r.translate(err)
	}
	return identity, true, nil
}

// ReplacePasswordHash is a compare-and-set: it stores nextHash only while
// the account is active and still holds previousHash, and reports whether it
// did.
func (r *Repository) ReplacePasswordHash(ctx context.Context, subjectID uuid.UUID, previousHash, nextHash string) (bool, error) {
	if !isV7(subjectID) {
		return false, errors.New("user ID must be a UUIDv7")
	}
	if previousHash == "" || nextHash == "" || previousHash == nextHash {
		return false, errors.New("password hash replacement requires distinct non-empty hashes")
	}
	result, err := r.db.ExecContext(ctx, r.q(`
		UPDATE forge_users
		SET password_hash = ?, updated_at = {now}
		WHERE id = ?
		  AND password_hash = ?
		  AND status = 'active'
	`), nextHash, subjectID, previousHash)
	return affectedOne(result, err, r.translate)
}

// CreateSession creates a session for an active membership that expires at
// expiresAt and returns its bearer token; only the token's digest is stored.
// It returns ErrMembershipRequired when the membership is not active.
func (r *Repository) CreateSession(ctx context.Context, membershipID uuid.UUID, expiresAt time.Time) (auth.Token, error) {
	return r.createSession(ctx, membershipID, 0, expiresAt)
}

// CreateSessionAtVersion creates a session only when credentialVersion is
// still the user's current version. This closes the race between credential
// verification and session persistence.
func (r *Repository) CreateSessionAtVersion(ctx context.Context, membershipID uuid.UUID, credentialVersion int64, expiresAt time.Time) (auth.Token, error) {
	if credentialVersion < 1 {
		return auth.Token{}, errors.New("credential version must be positive")
	}
	return r.createSession(ctx, membershipID, credentialVersion, expiresAt)
}

// createSession is CreateSession, and CreateSessionAtVersion when
// credentialVersion is positive.
func (r *Repository) createSession(ctx context.Context, membershipID uuid.UUID, credentialVersion int64, expiresAt time.Time) (auth.Token, error) {
	if !isV7(membershipID) {
		return auth.Token{}, errors.New("membership ID must be a UUIDv7")
	}
	if !expiresAt.After(r.now()) {
		return auth.Token{}, ErrExpiryRequired
	}
	token, err := auth.NewToken()
	if err != nil {
		return auth.Token{}, fmt.Errorf("generate session token: %w", err)
	}
	digest := token.Digest()
	id, err := uuid.New()
	if err != nil {
		return auth.Token{}, err
	}
	query := `
		INSERT INTO forge_sessions (id, token_digest, membership_id, credential_version, expires_at, last_seen_at, created_at)
		SELECT ?, ?, m.id, u.session_version, ?, {now}, {now}
		FROM forge_memberships m
		JOIN forge_users u ON u.id = m.user_id
		JOIN forge_tenants t ON t.id = m.tenant_id
		JOIN forge_organizations o ON o.id = t.organization_id
		WHERE m.id = ?` + activeChain
	args := []any{id, digest[:], r.dialect.Time(expiresAt), membershipID}
	if credentialVersion > 0 {
		query += `
		  AND u.session_version = ?`
		args = append(args, credentialVersion)
	}
	result, err := r.db.ExecContext(ctx, r.q(query), args...)
	created, err := affectedOne(result, err, r.translate)
	switch {
	case err != nil:
		return auth.Token{}, err
	case !created && credentialVersion > 0:
		return auth.Token{}, auth.ErrCredentialsInvalid
	case !created:
		return auth.Token{}, ErrMembershipRequired
	}
	return token, nil
}

// RevokeSession revokes the session with the token's digest; revoking an
// unknown or revoked session is not an error.
func (r *Repository) RevokeSession(ctx context.Context, token auth.Token) error {
	digest := token.Digest()
	_, err := r.db.ExecContext(ctx, r.q(`
		UPDATE forge_sessions
		SET revoked_at = COALESCE(revoked_at, {now})
		WHERE token_digest = ?
	`), digest[:])
	return r.translate(err)
}

// RevokeUserSessions revokes every session of the user and increments its
// session version, which also invalidates pending MFA challenges.
func (r *Repository) RevokeUserSessions(ctx context.Context, userID uuid.UUID) error {
	if !isV7(userID) {
		return errors.New("user ID must be a UUIDv7")
	}
	return r.inTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, r.q(`
			UPDATE forge_users
			SET session_version = session_version + 1, updated_at = {now}
			WHERE id = ?
		`), userID)
		updated, err := affectedOne(result, err, r.translate)
		if err != nil {
			return err
		}
		if !updated {
			return r.translate(sql.ErrNoRows)
		}
		return r.revokeSessionsOf(ctx, tx, userID)
	})
}

// revokeSessionsOf revokes every live session of userID.
func (r *Repository) revokeSessionsOf(ctx context.Context, tx db, userID uuid.UUID) error {
	_, err := tx.ExecContext(ctx, r.q(`
		UPDATE forge_sessions
		SET revoked_at = {now}
		WHERE revoked_at IS NULL
		  AND membership_id IN (SELECT id FROM forge_memberships WHERE user_id = ?)
	`), userID)
	return r.translate(err)
}

// ResolveSession returns the live session for digest. A session that is
// revoked, unused for the idle timeout (WithSessionIdleTimeout), or whose
// user, membership, tenant or organization is no longer active is not found;
// the caller (auth.Middleware) checks the absolute expiry. Resolving records
// the use in last_seen_at at most once a minute per session, so most
// requests only read; that write, conditional on the stored value still being
// stale, makes concurrent requests record one use. The database clock decides
// both the idle limit and the throttle. A failed write fails closed: the
// error is returned.
func (r *Repository) ResolveSession(ctx context.Context, digest auth.Digest) (auth.Session, bool, error) {
	touch, idle := r.dialect.AgoArg(sessionTouchInterval), r.dialect.AgoArg(r.idleTimeout)
	// One row per permission (NULL when the membership has none), in one
	// statement, so the principal comes from one snapshot.
	rows, err := r.db.QueryContext(ctx, r.q(`
		SELECT u.id, t.id, s.expires_at, rp.permission,
		       CASE WHEN s.last_seen_at <= {ago} THEN 1 ELSE 0 END
		FROM forge_sessions s
		JOIN forge_memberships m ON m.id = s.membership_id
		JOIN forge_users u ON u.id = m.user_id
		JOIN forge_tenants t ON t.id = m.tenant_id
		JOIN forge_organizations o ON o.id = t.organization_id
		LEFT JOIN forge_membership_roles mr
			ON mr.membership_id = m.id AND mr.tenant_id = m.tenant_id
		LEFT JOIN forge_role_permissions rp ON rp.role_id = mr.role_id
		WHERE s.token_digest = ?
		  AND s.revoked_at IS NULL
		  AND s.last_seen_at > {ago}
		  AND s.credential_version = u.session_version`+activeChain), touch, digest[:], idle)
	if err != nil {
		return auth.Session{}, false, r.translate(err)
	}
	defer func() { _ = rows.Close() }()
	var (
		found       bool
		subjectID   uuid.UUID
		tenantID    uuid.UUID
		expiresAt   Timestamp
		stale       bool
		permissions []auth.Permission
	)
	for rows.Next() {
		var permission sql.NullString
		if err := rows.Scan(&subjectID, &tenantID, &expiresAt, &permission, &stale); err != nil {
			return auth.Session{}, false, r.translate(err)
		}
		found = true
		if permission.Valid {
			parsed, err := auth.NewPermission(permission.String)
			if err != nil {
				return auth.Session{}, false, fmt.Errorf("invalid permission %q in identity store: %w", permission.String, err)
			}
			permissions = append(permissions, parsed)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return auth.Session{}, false, r.translate(err)
	}
	if !found {
		return auth.Session{}, false, nil
	}
	if stale {
		// Only a session that is still live is touched: one revoked or gone
		// idle meanwhile keeps its last use.
		if _, err := r.db.ExecContext(ctx, r.q(`
			UPDATE forge_sessions
			SET last_seen_at = {now}
			WHERE token_digest = ?
			  AND revoked_at IS NULL
			  AND last_seen_at > {ago}
			  AND last_seen_at <= {ago}
		`), digest[:], idle, touch); err != nil {
			return auth.Session{}, false, r.translate(err)
		}
	}
	tenant, err := tenancy.New(tenantID)
	if err != nil {
		return auth.Session{}, false, fmt.Errorf("invalid tenant in identity store: %w", err)
	}
	principal, err := auth.NewPrincipal(subjectID, tenant, permissions...)
	if err != nil {
		return auth.Session{}, false, fmt.Errorf("invalid principal in identity store: %w", err)
	}
	return auth.Session{Principal: principal, ExpiresAt: expiresAt.Time}, true, nil
}

// isV7 reports whether id is a UUIDv7 (version 7, RFC 9562 variant).
func isV7(id uuid.UUID) bool { return id.Version() == 7 && id.Variant() == 2 }

// affectedOne reports whether a statement changed exactly one row.
func affectedOne(result sql.Result, err error, translate func(error) error) (bool, error) {
	if err != nil {
		return false, translate(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, translate(err)
	}
	return affected == 1, nil
}

// affected is how many rows a statement changed.
func affected(result sql.Result, err error, translate func(error) error) (int64, error) {
	if err != nil {
		return 0, translate(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, translate(err)
	}
	return count, nil
}
