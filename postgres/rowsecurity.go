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

// RequireRowSecurity makes Open fail in production (environment is
// config.Production) when the role FORGE_DATABASE_URL connects as bypasses
// row-level security: a superuser, a role with BYPASSRLS, or a member of
// such a role (which can SET ROLE to it). Tenant isolation through RLS
// policies is then only as strong as every query's own predicate, so the
// application refuses to serve instead. In other environments it does
// nothing, so local tools may connect as an administrator.
//
// Pass it where the application opens its serving pool:
//
//	postgres.Open(ctx, app.Config().Database, postgres.RequireRowSecurity(app.Config().Environment))
//
// Serve as a NOSUPERUSER NOBYPASSRLS runtime role that owns no table and
// migrate as a separate role (FORGE_MIGRATION_DATABASE_URL); see
// docs/DATABASE.md.
func RequireRowSecurity(environment config.Environment) Option {
	return func(o *openOptions) { o.rowSecurity = environment == config.Production }
}

// roleEscapeQuery reads how current_user escapes row-level security, if
// it does: its own SUPERUSER or BYPASSRLS attribute, or membership in a role
// with either (role attributes are not inherited, but a member can SET ROLE).
const roleEscapeQuery = `
SELECT u.rolname, u.rolsuper, u.rolbypassrls,
       EXISTS (SELECT 1 FROM pg_roles r
               WHERE (r.rolsuper OR r.rolbypassrls) AND r.oid <> u.oid
                 AND pg_has_role(u.oid, r.oid, 'MEMBER'))
FROM pg_roles u
WHERE u.rolname = current_user`

func refuseRowSecurityBypass(ctx context.Context, pool *pgxpool.Pool) error {
	var role string
	var superuser, bypass, member bool
	if err := pool.QueryRow(ctx, roleEscapeQuery).Scan(&role, &superuser, &bypass, &member); err != nil {
		return fmt.Errorf("check the database role's row-level security: %w", err)
	}
	var reason string
	switch {
	case superuser:
		reason = "is a superuser"
	case bypass:
		reason = "has BYPASSRLS"
	case member:
		reason = "can SET ROLE to a superuser or BYPASSRLS role"
	default:
		return nil
	}
	return fmt.Errorf("%w: production refuses to serve as PostgreSQL role %q, which %s; "+
		"set FORGE_DATABASE_URL to a NOSUPERUSER NOBYPASSRLS runtime role with only data privileges "+
		"and migrate with FORGE_MIGRATION_DATABASE_URL (docs/DATABASE.md, \"Database roles\")", ErrRowSecurityBypass, role, reason)
}
