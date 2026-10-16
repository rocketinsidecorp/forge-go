package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestRepositoryResolvesCurrentIdentityAndRevocation(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	userID, orgID, tenantID, membershipID, roleID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	insertIdentityFixture(t, db, userID, orgID, tenantID, membershipID, "Alice@example.com", "alice@example.com", "Acme", "acme")
	if _, err := db.Exec(ctx, "INSERT INTO forge_roles (id, tenant_id, name) VALUES ($1, $2, 'reader')", roleID, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_permissions (name) VALUES ('tasks:read')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_role_permissions (role_id, permission) VALUES ($1, 'tasks:read')", roleID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_membership_roles (membership_id, role_id, tenant_id) VALUES ($1, $2, $3)", membershipID, roleID, tenantID); err != nil {
		t.Fatal(err)
	}

	token, err := repo.CreateSession(ctx, membershipID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	session, found, err := repo.ResolveSession(ctx, token.Digest())
	if err != nil || !found {
		t.Fatalf("ResolveSession() found=%v err=%v", found, err)
	}
	if session.Principal.SubjectID() != userID || session.Principal.Tenant().ID != tenantID || !session.Principal.Can(auth.Permission("tasks:read")) {
		t.Fatalf("unexpected principal: subject=%s tenant=%s permissions=%v", session.Principal.SubjectID(), session.Principal.Tenant().ID, session.Principal.Permissions())
	}

	if _, err := db.Exec(ctx, "DELETE FROM forge_role_permissions WHERE role_id = $1 AND permission = 'tasks:read'", roleID); err != nil {
		t.Fatal(err)
	}
	session, found, err = repo.ResolveSession(ctx, token.Digest())
	if err != nil || !found || session.Principal.Can(auth.Permission("tasks:read")) {
		t.Fatalf("permission change not reflected: found=%v permissions=%v err=%v", found, session.Principal.Permissions(), err)
	}

	if err := repo.RevokeSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.ResolveSession(ctx, token.Digest()); err != nil || found {
		t.Fatalf("revoked session found=%v err=%v", found, err)
	}
	if err := repo.RevokeSession(ctx, token); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryFailsClosedForInactiveIdentityAndCredentialVersion(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	insertIdentityFixture(t, db, userID, orgID, tenantID, membershipID, "bob@example.com", "bob@example.com", "Example", "example")

	token, err := repo.CreateSession(ctx, membershipID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RevokeUserSessions(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.ResolveSession(ctx, token.Digest()); err != nil || found {
		t.Fatalf("stale credential version found=%v err=%v", found, err)
	}

	if _, err := db.Exec(ctx, "UPDATE forge_memberships SET status = 'suspended' WHERE id = $1", membershipID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateSession(ctx, membershipID, time.Now().Add(time.Hour)); !errors.Is(err, authpostgres.ErrMembershipRequired) {
		t.Fatalf("inactive membership error=%v", err)
	}
}

func insertIdentityFixture(
	t *testing.T,
	db *postgres.DB,
	userID, orgID, tenantID, membershipID uuid.UUID,
	email, normalized, organization, slug string,
) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Exec(ctx, "INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, $2, $3, 'test-only')", userID, email, normalized); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_organizations (id, name) VALUES ($1, $2)", orgID, organization); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_tenants (id, organization_id, slug) VALUES ($1, $2, $3)", tenantID, orgID, slug); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES ($1, $2, $3)", membershipID, userID, tenantID); err != nil {
		t.Fatal(err)
	}
}

func TestLoginServiceAgainstPostgresRepository(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	passwordHash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, $2, $3, $4)", userID, "Alice@example.com", "alice@example.com", passwordHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_organizations (id, name) VALUES ($1, $2)", orgID, "Acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_tenants (id, organization_id, slug) VALUES ($1, $2, $3)", tenantID, orgID, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES ($1, $2, $3)", membershipID, userID, tenantID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	login, err := auth.NewLoginService(repo, repo, auth.WithLoginClock(func() time.Time { return now }), auth.WithSessionTTL(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	result, err := login.Login(ctx, auth.LoginInput{
		Email:      " ALICE@EXAMPLE.COM ",
		Password:   "correct horse battery staple",
		TenantSlug: "ACME",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("expires_at=%v", result.ExpiresAt)
	}
	session, found, err := repo.ResolveSession(ctx, result.Token.Digest())
	if err != nil || !found {
		t.Fatalf("ResolveSession() found=%v err=%v", found, err)
	}
	if session.Principal.SubjectID() != userID || session.Principal.Tenant().ID != tenantID {
		t.Fatalf("principal subject=%s tenant=%s", session.Principal.SubjectID(), session.Principal.Tenant().ID)
	}

	if _, err := login.Login(ctx, auth.LoginInput{
		Email:      "alice@example.com",
		Password:   "correct horse battery staple",
		TenantSlug: "other",
	}); !errors.Is(err, auth.ErrCredentialsInvalid) {
		t.Fatalf("wrong tenant error=%v", err)
	}
}

func TestCreateSessionAtVersionRejectsCredentialRace(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	passwordHash, err := auth.HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, 'race@example.com', 'race@example.com', $2)", userID, passwordHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_organizations (id, name) VALUES ($1, 'Race')", orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_tenants (id, organization_id, slug) VALUES ($1, $2, 'race')", tenantID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES ($1, $2, $3)", membershipID, userID, tenantID); err != nil {
		t.Fatal(err)
	}

	identity, found, err := repo.LookupPassword(ctx, "race@example.com", "race")
	if err != nil || !found {
		t.Fatalf("LookupPassword() found=%v err=%v", found, err)
	}
	if identity.CredentialVersion != 1 {
		t.Fatalf("credential version=%d", identity.CredentialVersion)
	}

	if err := repo.RevokeUserSessions(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateSessionAtVersion(ctx, membershipID, identity.CredentialVersion, time.Now().Add(time.Hour)); !errors.Is(err, auth.ErrCredentialsInvalid) {
		t.Fatalf("stale verified credential created session: %v", err)
	}

	fresh, found, err := repo.LookupPassword(ctx, "race@example.com", "race")
	if err != nil || !found {
		t.Fatalf("fresh LookupPassword() found=%v err=%v", found, err)
	}
	if fresh.CredentialVersion != 2 {
		t.Fatalf("fresh credential version=%d", fresh.CredentialVersion)
	}
	if _, err := repo.CreateSessionAtVersion(ctx, membershipID, fresh.CredentialVersion, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("fresh credential session: %v", err)
	}
}
