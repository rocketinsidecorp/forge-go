package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rocketinsidecorp/forge-go/config"
)

// ErrRowSecurityBypass is returned by Open with RequireRowSecurity when the
// connection's role would not be subject to row-level security.
var ErrRowSecurityBypass = errors.New("the database role bypasses row-level security")

// RequireRowSecurity makes Open fail, in every environment except
// development and test (so in staging and production), when the connection
// could escape row-level security. Both the role FORGE_DATABASE_URL logs in
// as (session_user) and the role it acts as (current_user) are checked, and
// Open fails when either
//
//   - is a superuser or has BYPASSRLS,
//   - is a member of such a role (it can SET ROLE to it), or
//   - has CREATEROLE (before PostgreSQL 16 it can grant itself such a
//     membership),
//
// or when the two differ: a role setting (`options=-c role=...`, ALTER ROLE
// ... SET role) makes the session act as another role, and `SET ROLE NONE`
// returns it to the login role, so only the login role counts and it must be
// the serving role itself. Tenant isolation through RLS policies is otherwise
// only as strong as every query's own predicate, so the application refuses
// to serve instead. In development and test it does nothing, so local tools
// may connect as an administrator.
//
// Pass it where the application opens its serving pool:
//
//	postgres.Open(ctx, app.Config().Database, postgres.RequireRowSecurity(app.Config().Environment))
//
// Serve as a NOSUPERUSER NOBYPASSRLS NOCREATEROLE runtime role that owns no
// table and migrate as a separate role (FORGE_MIGRATION_DATABASE_URL); see
// docs/DATABASE.md.
func RequireRowSecurity(environment config.Environment) Option {
	return func(o *openOptions) {
		o.rowSecurity = environment != config.Development && environment != config.Test
	}
}

// roleEscapeQuery describes the login role (session_user) and the role in
// effect (current_user), login role first: their own SUPERUSER, BYPASSRLS and
// CREATEROLE attributes, and membership in a role with SUPERUSER or BYPASSRLS
// (attributes are not inherited, but a member can SET ROLE).
const roleEscapeQuery = `
SELECT u.rolname, u.rolname = session_user, u.rolsuper, u.rolbypassrls, u.rolcreaterole,
       EXISTS (SELECT 1 FROM pg_roles r
               WHERE (r.rolsuper OR r.rolbypassrls) AND r.oid <> u.oid
                 AND pg_has_role(u.oid, r.oid, 'MEMBER'))
FROM pg_roles u
WHERE u.rolname IN (session_user, current_user)
ORDER BY u.rolname = session_user DESC`

func refuseRowSecurityBypass(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, roleEscapeQuery)
	if err != nil {
		return fmt.Errorf("check the database role's row-level security: %w", err)
	}
	defer rows.Close()
	var login, acting string
	refusal := ""
	for rows.Next() {
		var role string
		var isLogin, superuser, bypass, createRole, member bool
		if err := rows.Scan(&role, &isLogin, &superuser, &bypass, &createRole, &member); err != nil {
			return fmt.Errorf("check the database role's row-level security: %w", err)
		}
		if isLogin {
			login = role
		} else {
			acting = role
		}
		var reason string
		switch {
		case superuser:
			reason = "is a superuser"
		case bypass:
			reason = "has BYPASSRLS"
		case createRole:
			reason = "has CREATEROLE"
		case member:
			reason = "can SET ROLE to a superuser or BYPASSRLS role"
		}
		if reason != "" && refusal == "" {
			refusal = fmt.Sprintf("PostgreSQL role %q, which %s", role, reason)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check the database role's row-level security: %w", err)
	}
	if refusal == "" && acting != "" {
		refusal = fmt.Sprintf("PostgreSQL role %q acting as %q (a role setting such as options=-c role=), "+
			"which SET ROLE NONE undoes", login, acting)
	}
	if refusal == "" {
		return nil
	}
	return fmt.Errorf("%w: refusing to serve outside development and test as %s; "+
		"set FORGE_DATABASE_URL to log in directly as a NOSUPERUSER NOBYPASSRLS NOCREATEROLE runtime role "+
		"with only data privileges and migrate with FORGE_MIGRATION_DATABASE_URL (docs/DATABASE.md, \"Database roles\")",
		ErrRowSecurityBypass, refusal)
}
