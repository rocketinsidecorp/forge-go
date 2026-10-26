package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/config"
)

// ErrRowSecurityBypass is returned by Open, outside development and test,
// when the connection's role would not be subject to row-level security (see
// RequireRowSecurity).
var ErrRowSecurityBypass = errors.New("the database role bypasses row-level security")

// RequireRowSecurity makes Open check the role, as described below, when
// environment is neither development nor test (so in staging and
// production), even if cfg.Environment says development or test. It only
// ever adds the check: RequireRowSecurity(config.Development) leaves Open's
// default, which follows cfg.Environment, and it also wins over
// AllowRowSecurityBypass. Open already checks for cfg.Environment, which
// config.Load and forge.New set; pass this option only for a
// config.Database built in code whose Environment is not the
// application's.
//
// The check covers both the role FORGE_DATABASE_URL logs in as
// (session_user) and the role it acts as (current_user), and Open fails when
// either
//
//   - is a superuser or has BYPASSRLS or REPLICATION (logical decoding reads
//     every tenant's changes),
//   - is a member of such a role (it can SET ROLE to it),
//   - has CREATEROLE (before PostgreSQL 16 it can grant itself such a
//     membership),
//   - is a member of pg_execute_server_program, pg_write_server_files or
//     pg_read_server_files (paths to superuser or to the data files),
//   - owns, or is a member of the owner of, a table of this database with
//     row-level security enabled (an owner can ALTER TABLE ... NO FORCE ROW
//     LEVEL SECURITY), or
//   - holds TRUNCATE, REFERENCES (on the table or on any of its columns) or
//     TRIGGER on such a table, itself, through PUBLIC or through a role it
//     is a member of (none of the three is subject to row-level security:
//     TRUNCATE empties every tenant's rows, a foreign key probes them and a
//     trigger sees them; the usual cause is a GRANT ALL), or
//   - holds any privilege on a partition that has no row-level security of
//     its own but belongs to a partitioned table with it (querying the
//     partition directly skips the parent's policies).
//
// Membership counts whatever its INHERIT and SET options: a role granted
// WITH INHERIT FALSE, SET FALSE cannot use the other role's privileges, and
// is refused all the same. That over-refusal is deliberate (it fails closed)
// and has a simple fix: do not grant the serving role such memberships.
//
// It also fails when the two differ: a role setting (`options=-c role=...`, ALTER ROLE
// ... SET role) makes the session act as another role, and `SET ROLE NONE`
// returns it to the login role, so only the login role counts and it must be
// the serving role itself. Tenant isolation through RLS policies is otherwise
// only as strong as every query's own predicate, so the application refuses
// to serve instead. Outside these environments, and without this option,
// development and test are not checked, so local tools may connect as an
// administrator.
//
// Serve as a NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOREPLICATION runtime role
// that owns no table and migrate as a separate role (FORGE_MIGRATION_DATABASE_URL); see
// docs/DATABASE.md.
func RequireRowSecurity(environment config.Environment) Option {
	require := environment != config.Development && environment != config.Test
	return func(o *openOptions) { o.requireRowSecurity = o.requireRowSecurity || require }
}

// AllowRowSecurityBypass is the explicit opt-out of the check Open applies
// outside development and test (see RequireRowSecurity): Open then accepts
// any role. Pass it only for a pool that must connect as such a role on
// purpose, such as an administration or reporting tool, never for the pool
// an application serves tenants with. It is the only opt-out, and
// RequireRowSecurity for a staging or production environment overrides it.
func AllowRowSecurityBypass() Option {
	return func(o *openOptions) { o.allowRowSecurityBypass = true }
}

// Lazy makes Open, when lazy is true, return without connecting: the pool
// connects on its first query, and each connection it opens is checked for
// row-level security (RequireRowSecurity) before its first use, where Open
// would check the pool. A wrong URL or an unreachable database is then
// reported by that query, not by Open. Generated applications pass
// Lazy(!app.Serving()): `routes` and `openapi` compose the application
// without serving it and need no reachable database. Lazy(false) is Open's
// default. A set PGOPTIONS is refused lazily too (see Open).
func Lazy(lazy bool) Option {
	return func(o *openOptions) { o.lazy = lazy }
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
// TABLE; pg_has_role(u, u) is true, so direct ownership counts too); and
// whether it, or a role it is a member of, holds TRUNCATE, REFERENCES or
// TRIGGER on such a table (has_table_privilege counts PUBLIC and inherited
// grants; the membership join adds roles it can only SET ROLE to, and
// has_any_column_privilege adds a column-level REFERENCES grant); and whether
// it, or a role it is a member of, holds any privilege on a partition without
// row-level security whose ancestors (pg_partition_ancestors) include a
// table with it. It runs with
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
                 AND pg_catalog.pg_has_role(u.oid, c.relowner, 'MEMBER')),
       EXISTS (SELECT 1 FROM pg_catalog.pg_class c, pg_catalog.pg_roles r
               WHERE c.relrowsecurity
                 AND pg_catalog.pg_has_role(u.oid, r.oid, 'MEMBER')
                 AND (pg_catalog.has_table_privilege(r.oid, c.oid, 'TRUNCATE, REFERENCES, TRIGGER')
                      OR pg_catalog.has_any_column_privilege(r.oid, c.oid, 'REFERENCES'))),
       EXISTS (SELECT 1 FROM pg_catalog.pg_class c, pg_catalog.pg_roles r
               WHERE c.relispartition AND NOT c.relrowsecurity
                 AND EXISTS (SELECT 1 FROM pg_catalog.pg_partition_ancestors(c.oid) a
                             JOIN pg_catalog.pg_class p ON p.oid = a.relid
                             WHERE p.relrowsecurity)
                 AND pg_catalog.pg_has_role(u.oid, r.oid, 'MEMBER')
                 AND (pg_catalog.has_table_privilege(r.oid, c.oid,
                          'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')
                      OR pg_catalog.has_any_column_privilege(r.oid, c.oid, 'SELECT, INSERT, UPDATE, REFERENCES')))
FROM pg_catalog.pg_roles u
WHERE u.rolname IN (session_user, current_user)
ORDER BY u.rolname = session_user DESC`

// txStarter is a pool, or one connection of a lazy pool.
type txStarter interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// refuseRowSecurityBypass runs roleEscapeQuery in a read-only transaction
// whose search_path is pg_catalog alone; the connection's own search_path,
// which the URL can set, is back after the rollback. Any failure, including
// starting the transaction or pinning the path, refuses.
func refuseRowSecurityBypass(ctx context.Context, db txStarter) error {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
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
		var isLogin, superuser, bypass, createRole, replication, member, serverFiles, ownsRLS, unconfined, partition bool
		if err := rows.Scan(&role, &isLogin, &superuser, &bypass, &createRole, &replication,
			&member, &serverFiles, &ownsRLS, &unconfined, &partition); err != nil {
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
		case unconfined:
			reason = "holds TRUNCATE, REFERENCES or TRIGGER, which row-level security does not confine, " +
				"on a table with row-level security, itself, through PUBLIC or through a role it belongs to " +
				"(REVOKE TRUNCATE, REFERENCES, TRIGGER ON ALL TABLES IN SCHEMA public FROM each of them)"
		case partition:
			reason = "holds a privilege on a partition without row-level security of a table with row-level security, " +
				"so querying the partition directly skips the parent's policies " +
				"(ENABLE and FORCE ROW LEVEL SECURITY with the same policies on every partition, or revoke the privileges on it)"
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
