package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/migrate"
)

// migrationLockKey serializes migrators across processes and replicas.
var migrationLockKey = LockKey("forge:schema_migrations")

// NewMigrator returns a migrator for the database cfg points at. It opens its
// own dedicated connections, never ones from an application pool; close it
// when done.
func NewMigrator(cfg config.Database, migrations []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
	poolConfig, err := parseConfig(cfg)
	if err != nil {
		return nil, err
	}
	return migrate.New(stdlib.OpenDB(*poolConfig.ConnConfig), dialect{}, migrations, logger)
}

// Commands returns the migrate and rollback subcommands for an application
// binary with one migration set; see migrate.Commands.
func Commands(migrations fs.FS) []forge.Command {
	return CommandsFromSets(migrations)
}

// CommandsFromSets returns migrate and rollback commands over one globally
// ordered history composed from independent migration filesystems.
func CommandsFromSets(migrationSets ...fs.FS) []forge.Command {
	return migrate.CommandsFromSets(migrationSets, func(_ context.Context, cfg config.Database, loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return NewMigrator(cfg, loaded, logger)
	})
}

// dialect is PostgreSQL for the migration engine.
type dialect struct{}

func (dialect) Placeholder(n int) string { return "$" + strconv.Itoa(n) }

func (dialect) PrepareSession(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "SET statement_timeout = 0")
	return err
}

// ExplainScriptError makes a migration that calls uuidv7() on a server
// without it (PostgreSQL 17 and older) say so: SQLSTATE 42883
// (undefined_function) naming uuidv7 gets the server version and the two
// ways out. The failed migration's transaction has been rolled back.
func (dialect) ExplainScriptError(ctx context.Context, q migrate.Queryer, err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42883" || !strings.Contains(strings.ToLower(pgErr.Message), "uuidv7") {
		return err
	}
	version := "unknown"
	var name string
	if q.QueryRowContext(ctx, "SELECT current_setting('server_version')").Scan(&name) == nil {
		version = name
	}
	return fmt.Errorf("%w (this server is PostgreSQL %s: uuidv7() needs PostgreSQL 18, "+
		"or a uuidv7() function defined by an earlier migration)", err, version)
}

func (dialect) TryLock(ctx context.Context, conn *sql.Conn) (bool, error) {
	var acquired bool
	err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", migrationLockKey).Scan(&acquired)
	return acquired, err
}

func (dialect) Unlock(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey)
	return err
}

// SessionIdentity is the database and search_path the bookkeeping relies on.
func (dialect) SessionIdentity(ctx context.Context, q migrate.Queryer) (string, error) {
	var identity string
	err := q.QueryRowContext(ctx, "SELECT current_database() || ' search_path=' || current_setting('search_path')").Scan(&identity)
	return identity, err
}

func (dialect) TableExists(ctx context.Context, conn *sql.Conn, table string) (bool, error) {
	var exists bool
	err := conn.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists)
	return exists, err
}
