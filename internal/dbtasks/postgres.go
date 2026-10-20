package dbtasks

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	pgInvalidCatalogName = "3D000"
	pgDuplicateDatabase  = "42P04"
	pgUndefinedTable     = "42P01"
	// forceDropVersion is PostgreSQL 13, which added DROP DATABASE ... WITH (FORCE).
	forceDropVersion = 130000
)

// pgConnect connects with the administration credentials to database name.
func (t *Target) pgConnect(ctx context.Context, name string) (*pgx.Conn, error) {
	u := *t.priv
	u.Path, u.RawPath = "/"+name, ""
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		// Not wrapped: parse errors can quote the URL.
		return nil, errors.New("the PostgreSQL connection URL is not valid")
	}
	cfg.ConnectTimeout = t.connectTimeout()
	return pgx.ConnectConfig(ctx, cfg)
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
	)
	if c.port != "" {
		env = append(env, "PGPORT="+c.port)
	}
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
