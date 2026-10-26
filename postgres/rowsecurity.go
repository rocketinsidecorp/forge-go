package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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
//   - is a superuser or has BYPASSRLS or REPLICATION (logical decoding reads
//     every tenant's changes),
//   - is a member of such a role (it can SET ROLE to it),
//   - has CREATEROLE (before PostgreSQL 16 it can grant itself such a
//     membership),
//   - is a member of pg_execute_server_program, pg_write_server_files or
//     pg_read_server_files (paths to superuser or to the data files), or
//   - owns, or is a member of the owner of, a table of this database with
//     row-level security enabled (an owner can ALTER TABLE ... NO FORCE ROW
//     LEVEL SECURITY),
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
// Serve as a NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOREPLICATION runtime role
// that owns no table and migrate as a separate role (FORGE_MIGRATION_DATABASE_URL); see
// docs/DATABASE.md.
func RequireRowSecurity(environment config.Environment) Option {
	return func(o *openOptions) {
		o.rowSecurity = environment != config.Development && environment != config.Test
	}
}

// roleEscapeQuery describes the login role (session_user) and the role in
// effect (current_user), login role first: their own SUPERUSER, BYPASSRLS,
// CREATEROLE and REPLICATION attributes; membership in another role with
// SUPERUSER, BYPASSRLS or REPLICATION (attributes are not inherited, but a
// member can SET ROLE); membership in a predefined role that reaches the
// server's files or programs (pg_execute_server_program, pg_write_server_files,
// pg_read_server_files: a path to superuser or to every tenant's data); and
// whether it owns, or is a member of the owner of, a table of this database
// with row-level security enabled (an owner can switch FORCE off with ALTER
// TABLE; pg_has_role(u, u) is true, so direct ownership counts too). It runs with
// search_path pinned to pg_catalog (see refuseRowSecurityBypass), so the
// operators (=, IN, <>) and catalog names resolve to PostgreSQL's own, never
// to a function, operator or view a search_path setting would put first;
// the catalog names are also qualified, as defense in depth.
const roleEscapeQuery = `
SELECT u.rolname, u.rolname = session_user, u.rolsuper, u.rolbypassrls, u.rolcreaterole,
       u.rolreplication,
       EXISTS (SELECT 1 FROM pg_catalog.pg_roles r
               WHERE (r.rolsuper OR r.rolbypassrls OR r.rolreplication) AND r.oid <> u.oid
                 AND pg_catalog.pg_has_role(u.oid, r.oid, 'MEMBER')),
       EXISTS (SELECT 1 FROM pg_catalog.pg_roles r
               WHERE r.rolname IN ('pg_execute_server_program', 'pg_write_server_files',
                                   'pg_read_server_files')
                 AND pg_catalog.pg_has_role(u.oid, r.oid, 'MEMBER')),
       EXISTS (SELECT 1 FROM pg_catalog.pg_class c
               WHERE c.relrowsecurity
                 AND pg_catalog.pg_has_role(u.oid, c.relowner, 'MEMBER'))
FROM pg_catalog.pg_roles u
WHERE u.rolname IN (session_user, current_user)
ORDER BY u.rolname = session_user DESC`

// refuseRowSecurityBypass runs roleEscapeQuery in a read-only transaction
// whose search_path is pg_catalog alone; the connection's own search_path,
// which the URL can set, is back after the rollback. Any failure, including
// starting the transaction or pinning the path, refuses.
func refuseRowSecurityBypass(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("check the database role's row-level security: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return fmt.Errorf("check the database role's row-level security: %w", err)
	}
	rows, err := tx.Query(ctx, roleEscapeQuery)
	if err != nil {
		return fmt.Errorf("check the database role's row-level security: %w", err)
	}
	defer rows.Close()
	var login, acting string
	refusal := ""
	for rows.Next() {
		var role string
		var isLogin, superuser, bypass, createRole, replication, member, serverFiles, ownsRLS bool
		if err := rows.Scan(&role, &isLogin, &superuser, &bypass, &createRole, &replication,
			&member, &serverFiles, &ownsRLS); err != nil {
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
		case replication:
			reason = "has REPLICATION"
		case member:
			reason = "can SET ROLE to a superuser, BYPASSRLS or REPLICATION role"
		case serverFiles:
			reason = "is a member of pg_execute_server_program, pg_write_server_files or pg_read_server_files"
		case ownsRLS:
			reason = "owns, or can SET ROLE to the owner of, a table with row-level security"
		}
		if reason != "" && refusal == "" {
			refusal = fmt.Sprintf("PostgreSQL role %q, which %s", role, reason)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check the database role's row-level security: %w", err)
	}
	// Fail closed: the query must find the login role. pg_roles always holds
	// session_user, so an empty result means something is wrong (a catalog
	// that cannot be read as expected), never that the role is harmless.
	if login == "" {
		return fmt.Errorf("%w: refusing to serve outside development and test: "+
			"the role FORGE_DATABASE_URL logs in as was not found in pg_roles", ErrRowSecurityBypass)
	}
	if refusal == "" && acting != "" {
		refusal = fmt.Sprintf("PostgreSQL role %q acting as %q (a role setting such as options=-c role=), "+
			"which SET ROLE NONE undoes", login, acting)
	}
	if refusal == "" {
		return nil
	}
	return fmt.Errorf("%w: refusing to serve outside development and test as %s; "+
		"set FORGE_DATABASE_URL to log in directly as a NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOREPLICATION "+
		"runtime role that owns no table and has only data privileges, and migrate with FORGE_MIGRATION_DATABASE_URL (docs/DATABASE.md, \"Database roles\")",
		ErrRowSecurityBypass, refusal)
}
