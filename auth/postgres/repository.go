package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var (
	ErrMembershipRequired                               = errors.New("active membership is required")
	ErrExpiryRequired                                   = errors.New("session expiry must be in the future")
	_                     auth.SessionCreator           = (*Repository)(nil)
	_                     auth.CredentialSessionCreator = (*Repository)(nil)
)

type Repository struct {
	db  *postgres.DB
	now func() time.Time
}

func New(db *postgres.DB) (*Repository, error) {
	if db == nil {
		return nil, errors.New("authentication database is required")
	}
	return &Repository{db: db, now: time.Now}, nil
}

func (r *Repository) LookupPassword(ctx context.Context, emailNormalized, tenantSlug string) (auth.PasswordIdentity, bool, error) {
	var identity auth.PasswordIdentity
	err := r.db.QueryRow(ctx, `
		SELECT u.id, m.id, u.password_hash, u.session_version,
		       EXISTS (SELECT 1 FROM forge_totp_factors f WHERE f.user_id = u.id)
		FROM forge_users u
		JOIN forge_memberships m ON m.user_id = u.id
		JOIN forge_tenants t ON t.id = m.tenant_id
		JOIN forge_organizations o ON o.id = t.organization_id
		WHERE u.email_normalized = $1
		  AND t.slug = $2
		  AND u.status = 'active'
		  AND m.status = 'active'
		  AND t.status = 'active'
		  AND o.status = 'active'
	`, emailNormalized, tenantSlug).Scan(&identity.SubjectID, &identity.MembershipID, &identity.PasswordHash, &identity.CredentialVersion, &identity.MFARequired)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.PasswordIdentity{}, false, nil
	}
	if err != nil {
		return auth.PasswordIdentity{}, false, postgres.Translate(err)
	}
	return identity, true, nil
}

func (r *Repository) ReplacePasswordHash(ctx context.Context, subjectID uuid.UUID, previousHash, nextHash string) (bool, error) {
	if subjectID.Version() != 7 || subjectID.Variant() != 2 {
		return false, errors.New("user ID must be a UUIDv7")
	}
	if previousHash == "" || nextHash == "" || previousHash == nextHash {
		return false, errors.New("password hash replacement requires distinct non-empty hashes")
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE forge_users
		SET password_hash = $3, updated_at = now()
		WHERE id = $1
		  AND password_hash = $2
		  AND status = 'active'
	`, subjectID, previousHash, nextHash)
	if err != nil {
		return false, postgres.Translate(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *Repository) CreateSession(ctx context.Context, membershipID uuid.UUID, expiresAt time.Time) (auth.Token, error) {
	if membershipID.Version() != 7 || membershipID.Variant() != 2 {
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
	tag, err := r.db.Exec(ctx, `
		INSERT INTO forge_sessions (token_digest, membership_id, credential_version, expires_at)
		SELECT $1, m.id, u.session_version, $3
		FROM forge_memberships m
		JOIN forge_users u ON u.id = m.user_id
		JOIN forge_tenants t ON t.id = m.tenant_id
		JOIN forge_organizations o ON o.id = t.organization_id
		WHERE m.id = $2
		  AND m.status = 'active'
		  AND u.status = 'active'
		  AND t.status = 'active'
		  AND o.status = 'active'
	`, digest[:], membershipID, expiresAt.UTC())
	if err != nil {
		return auth.Token{}, postgres.Translate(err)
	}
	if tag.RowsAffected() == 0 {
		return auth.Token{}, ErrMembershipRequired
	}
	return token, nil
}

// CreateSessionAtVersion creates a session only when credentialVersion is still
// the user's current version. This closes the race between credential
// verification and session persistence.
func (r *Repository) CreateSessionAtVersion(
	ctx context.Context,
	membershipID uuid.UUID,
	credentialVersion int64,
	expiresAt time.Time,
) (auth.Token, error) {
	if membershipID.Version() != 7 || membershipID.Variant() != 2 {
		return auth.Token{}, errors.New("membership ID must be a UUIDv7")
	}
	if credentialVersion < 1 {
		return auth.Token{}, errors.New("credential version must be positive")
	}
	if !expiresAt.After(r.now()) {
		return auth.Token{}, ErrExpiryRequired
	}
	token, err := auth.NewToken()
	if err != nil {
		return auth.Token{}, fmt.Errorf("generate session token: %w", err)
	}
	digest := token.Digest()
	tag, err := r.db.Exec(ctx, `
		INSERT INTO forge_sessions (token_digest, membership_id, credential_version, expires_at)
		SELECT $1, m.id, u.session_version, $4
		FROM forge_memberships m
		JOIN forge_users u ON u.id = m.user_id
		JOIN forge_tenants t ON t.id = m.tenant_id
		JOIN forge_organizations o ON o.id = t.organization_id
		WHERE m.id = $2
		  AND u.session_version = $3
		  AND m.status = 'active'
		  AND u.status = 'active'
		  AND t.status = 'active'
		  AND o.status = 'active'
	`, digest[:], membershipID, credentialVersion, expiresAt.UTC())
	if err != nil {
		return auth.Token{}, postgres.Translate(err)
	}
	if tag.RowsAffected() != 1 {
		return auth.Token{}, auth.ErrCredentialsInvalid
	}
	return token, nil
}

func (r *Repository) RevokeSession(ctx context.Context, token auth.Token) error {
	digest := token.Digest()
	_, err := r.db.Exec(ctx, `
		UPDATE forge_sessions
		SET revoked_at = COALESCE(revoked_at, now())
		WHERE token_digest = $1
	`, digest[:])
	if err != nil {
		return postgres.Translate(err)
	}
	return nil
}

func (r *Repository) RevokeUserSessions(ctx context.Context, userID uuid.UUID) error {
	if userID.Version() != 7 || userID.Variant() != 2 {
		return errors.New("user ID must be a UUIDv7")
	}
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE forge_users
			SET session_version = session_version + 1, updated_at = now()
			WHERE id = $1
		`, userID)
		if err != nil {
			return postgres.Translate(err)
		}
		if tag.RowsAffected() == 0 {
			return postgres.Translate(pgx.ErrNoRows)
		}
		_, err = tx.Exec(ctx, `
			UPDATE forge_sessions s
			SET revoked_at = COALESCE(s.revoked_at, now())
			FROM forge_memberships m
			WHERE s.membership_id = m.id AND m.user_id = $1
		`, userID)
		return postgres.Translate(err)
	})
}

func (r *Repository) ResolveSession(ctx context.Context, digest auth.Digest) (auth.Session, bool, error) {
	var (
		subjectID       uuid.UUID
		tenantID        uuid.UUID
		expiresAt       time.Time
		permissionNames []string
	)
	err := r.db.QueryRow(ctx, `
		SELECT
			u.id,
			t.id,
			s.expires_at,
			COALESCE(array_agg(DISTINCT rp.permission) FILTER (WHERE rp.permission IS NOT NULL), ARRAY[]::text[])
		FROM forge_sessions s
		JOIN forge_memberships m ON m.id = s.membership_id
		JOIN forge_users u ON u.id = m.user_id
		JOIN forge_tenants t ON t.id = m.tenant_id
		JOIN forge_organizations o ON o.id = t.organization_id
		LEFT JOIN forge_membership_roles mr
			ON mr.membership_id = m.id AND mr.tenant_id = m.tenant_id
		LEFT JOIN forge_role_permissions rp ON rp.role_id = mr.role_id
		WHERE s.token_digest = $1
		  AND s.revoked_at IS NULL
		  AND s.credential_version = u.session_version
		  AND m.status = 'active'
		  AND u.status = 'active'
		  AND t.status = 'active'
		  AND o.status = 'active'
		GROUP BY u.id, t.id, s.expires_at
	`, digest[:]).Scan(&subjectID, &tenantID, &expiresAt, &permissionNames)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Session{}, false, nil
	}
	if err != nil {
		return auth.Session{}, false, postgres.Translate(err)
	}
	tenant, err := tenancy.New(tenantID)
	if err != nil {
		return auth.Session{}, false, fmt.Errorf("invalid tenant in identity store: %w", err)
	}
	permissions := make([]auth.Permission, 0, len(permissionNames))
	for _, name := range permissionNames {
		permission, err := auth.NewPermission(name)
		if err != nil {
			return auth.Session{}, false, fmt.Errorf("invalid permission %q in identity store: %w", name, err)
		}
		permissions = append(permissions, permission)
	}
	principal, err := auth.NewPrincipal(subjectID, tenant, permissions...)
	if err != nil {
		return auth.Session{}, false, fmt.Errorf("invalid principal in identity store: %w", err)
	}
	return auth.Session{Principal: principal, ExpiresAt: expiresAt.UTC()}, true, nil
}
