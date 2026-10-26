package dbtasks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rocketinsidecorp/forge-go/internal/pgurl"
)

const (
	pgInvalidCatalogName = "3D000"
	pgDuplicateDatabase  = "42P04"
	pgUndefinedTable     = "42P01"
	// forceDropVersion is PostgreSQL 13, which added DROP DATABASE ... WITH (FORCE).
	forceDropVersion = 130000
)

// pgDefaultPort is the port of a URL without one, for pgx and the tools alike.
const pgDefaultPort = "5432"

// pgOptionKeys are the URL parameters the administration commands pass to
// pgx: options that do not pick another server or database. service,
// servicefile, hostaddr and session settings (options among them: -c sets
// any) are refused by pgx; host, port and database (dbname is its alias) are
// allowed only because the URL's authority and path set them, and
// pgConnConfig pins their parsed values.
var pgOptionKeys = []string{
	"user", "password", "passfile", "connect_timeout", "application_name",
	"sslmode", "sslrootcert", "sslcert", "sslkey", "sslpassword", "sslnegotiation", "sslsni",
	"channel_binding", "require_auth", "min_protocol_version", "max_protocol_version", "krbsrvname", "krbspn",
}

// pgConnect connects with the administration credentials to database name.
func (t *Target) pgConnect(ctx context.Context, name string) (*pgx.Conn, error) {
	cfg, err := t.pgConnConfig(name)
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, cfg)
}

// pgConnConfig parses the administration URL for database name with pgx and
// checks that pgx will dial exactly the host, port and database that Local
// and the reserved-name check judged, rather than trusting net/url and pgx
// to read the URL alike. The port is made explicit so PGPORT in forge's own
// environment cannot change it and the tools (pgEnvironment) use the same.
func (t *Target) pgConnConfig(name string) (*pgx.ConnConfig, error) {
	u := *t.priv
	host, port := u.Hostname(), portOr(u.Port(), pgDefaultPort)
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, errors.New("the PostgreSQL connection URL has an invalid port")
	}
	u.Host = net.JoinHostPort(host, port)
	u.Path, u.RawPath = "/"+name, ""
	cfg, err := parsePgConfig(u.String())
	if err != nil {
		var refused *pgurl.RefusedError
		if errors.As(err, &refused) {
			return nil, refused
		}
		// Not wrapped: parse errors can quote the URL.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			// A TLS file (sslrootcert, sslcert, sslkey) pgx could not read; its
			// path is not echoed either.
			return nil, errors.New("the PostgreSQL connection URL has an invalid TLS setting: check that the files named by sslrootcert, sslcert and sslkey exist and are readable")
		}
		return nil, fmt.Errorf("the PostgreSQL connection URL is not valid, or has a parameter forge db commands do not accept (they accept %s)", strings.Join(pgOptionKeys, ", "))
	}
	ambiguous := errors.New("the PostgreSQL connection URL is ambiguous: PostgreSQL would connect to another server or database than the one forge checked")
	if cfg.Database != name {
		return nil, ambiguous
	}
	// sslmode=prefer and allow add a fallback attempt; every attempt must
	// dial the same address.
	attempts := append([]*pgconn.FallbackConfig{{Host: cfg.Host, Port: cfg.Port}}, cfg.Fallbacks...)
	for _, attempt := range attempts {
		if attempt.Host != host || uint64(attempt.Port) != portNumber {
			return nil, ambiguous
		}
	}
	cfg.ConnectTimeout = t.connectTimeout()
	return cfg, nil
}

// parsePgConfig parses an administration URL with pgx without letting forge's
// own environment decide anything. pgx reads PGSSLMODE, PGOPTIONS,
// PGTARGETSESSIONATTRS, PGSERVICE, PGUSER and every other PG* variable
// straight from the process environment as defaults for what the URL leaves
// unset, and has no option to ignore them. They are not unset around the
// parse (that would edit the environment of the whole process under every
// concurrent reader): pgurl.Isolate writes the libpq default of each such
// parameter the environment sets into the URL, which pgx prefers to the
// environment, and a service named by PGSERVICE is replaced by an empty one.
// A URL without sslmode therefore gets the libpq default (prefer) from both
// pgx and the vendor tools (pgEnvironment drops the variables for them),
// never a lower PGSSLMODE. A PG* variable naming a TLS file (PGSSLROOTCERT,
// PGSSLCERT, PGSSLKEY, PGSSLPASSWORD) that the URL does not name is refused:
// it has no neutral value, and ignoring it could weaken verification. Session settings the URL does not name (PGOPTIONS,
// PGTZ, PGAPPNAME) are removed after the parse.
func parsePgConfig(connString string) (*pgx.ConnConfig, error) {
	allowed := append([]string{"host", "port", "database"}, pgOptionKeys...)
	iso, err := pgurl.Isolate(connString, pgurl.Everything, allowed)
	if err != nil {
		var refused *pgurl.RefusedError
		if errors.As(err, &refused) {
			// Names variables and parameters, never a value or the URL.
			return nil, refused
		}
		return nil, errors.New("the PostgreSQL connection URL is not valid")
	}
	defer iso.Close()
	cfg, err := pgx.ParseConfigWithOptions(iso.URL, pgx.ParseConfigOptions{
		ParseConfigOptions: pgconn.ParseConfigOptions{ConnStringAllowedKeys: append(allowed, iso.PinnedKeys()...)},
	})
	if err != nil {
		return nil, err
	}
	iso.StripSession(cfg.RuntimeParams)
	return cfg, nil
}

// pgAdmin connects to a maintenance database: the one place from which the
// application's database can be created or dropped.
func (t *Target) pgAdmin(ctx context.Context) (*pgx.Conn, error) {
	var lastErr error
	for _, name := range []string{"postgres", "template1"} {
		conn, err := t.pgConnect(ctx, name)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgInvalidCatalogName {
			break
		}
	}
	return nil, fmt.Errorf("connect to the PostgreSQL server: %w", lastErr)
}

func (t *Target) pgExists(ctx context.Context) (bool, error) {
	conn, err := t.pgAdmin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	return pgDatabaseExists(ctx, conn, t.Name)
}

func pgDatabaseExists(ctx context.Context, conn *pgx.Conn, name string) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
	return exists, err
}

func (t *Target) pgCreate(ctx context.Context) (bool, error) {
	conn, err := t.pgAdmin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	exists, err := pgDatabaseExists(ctx, conn, t.Name)
	if err != nil || exists {
		return false, err
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{t.Name}.Sanitize()); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgDuplicateDatabase {
			return false, nil // created concurrently
		}
		return false, fmt.Errorf("create database: %w", err)
	}
	return true, nil
}

func (t *Target) pgDrop(ctx context.Context) (bool, error) {
	conn, err := t.pgAdmin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	exists, err := pgDatabaseExists(ctx, conn, t.Name)
	if err != nil || !exists {
		return false, err
	}
	identifier := pgx.Identifier{t.Name}.Sanitize()
	var versionText string
	if err := conn.QueryRow(ctx, "SHOW server_version_num").Scan(&versionText); err != nil {
		return false, fmt.Errorf("read server version: %w", err)
	}
	version, _ := strconv.Atoi(versionText)
	if version >= forceDropVersion {
		// FORCE ends sessions that still hold the database, such as a
		// development server that is running.
		_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)")
	} else {
		if _, err = conn.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", t.Name); err == nil {
			_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+identifier)
		}
	}
	if err != nil {
		return false, fmt.Errorf("drop database: %w", err)
	}
	return true, nil
}

// pgAppliedMigrations reads the migration bookkeeping of the target database.
//
// The returned table name is schema-qualified: pg_dump output ends with an
// empty search_path, under which an unqualified INSERT would not find it.
func (t *Target) pgAppliedMigrations(ctx context.Context) (applied []appliedMigration, table string, err error) {
	conn, err := t.pgConnect(ctx, t.Name)
	if err != nil {
		return nil, "", fmt.Errorf("connect to the database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var schema *string
	err = conn.QueryRow(ctx, `SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = to_regclass($1)`, migrationsTable).Scan(&schema)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && schema == nil) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	table = pgx.Identifier{*schema, migrationsTable}.Sanitize()
	rows, err := conn.Query(ctx, "SELECT version, name, checksum FROM "+table+" ORDER BY version")
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var m appliedMigration
		if err := rows.Scan(&m.version, &m.name, &m.checksum); err != nil {
			return nil, "", err
		}
		applied = append(applied, m)
	}
	return applied, table, rows.Err()
}

// pgEnvironment configures libpq tools through PG* variables, so the
// password is in the child's environment and never in its arguments. Any
// PG* variable already set (a service file, an options string, another
// password) is dropped so the tools cannot connect anywhere else.
func (t *Target) pgEnvironment(base []string) []string {
	c := credentialsOf(t.priv)
	env := withoutPrefix(base, "PG")
	env = append(env,
		"PGHOST="+c.host,
		"PGDATABASE="+t.Name,
		"PGAPPNAME=forge-cli",
		"PGCONNECT_TIMEOUT="+strconv.Itoa(max(1, int(t.connectTimeout().Seconds()))),
		// Explicit, like pgConnConfig's, so tools and pgx dial the same port.
		"PGPORT="+portOr(c.port, pgDefaultPort),
	)
	if c.user != "" {
		env = append(env, "PGUSER="+c.user)
	}
	if c.password != "" {
		env = append(env, "PGPASSWORD="+c.password)
	}
	for param, variable := range map[string]string{
		"sslmode": "PGSSLMODE", "sslrootcert": "PGSSLROOTCERT", "sslcert": "PGSSLCERT", "sslkey": "PGSSLKEY",
	} {
		if value := c.params.Get(param); value != "" {
			env = append(env, variable+"="+value)
		}
	}
	return env
}

func withoutPrefix(env []string, prefix string) []string {
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}
