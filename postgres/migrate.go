package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

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
	return migrate.New(stdlib.OpenDB(*poolConfig.ConnConfig), dialect{uuidv7: uuidv7Migrations(migrations)}, migrations, logger)
}

// uuidv7Call matches a call of PostgreSQL 18's uuidv7() function.
var (
	uuidv7Call  = regexp.MustCompile(`(?i)\buuidv7\s*\(`)
	sqlComments = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/`)
)

// uuidv7Migrations names the migrations whose up script calls uuidv7(),
// comments aside: Forge's identity tables use it as a column default.
func uuidv7Migrations(migrations []migrate.Migration) []string {
	var names []string
	for _, migration := range migrations {
		if uuidv7Call.MatchString(sqlComments.ReplaceAllString(migration.Up.SQL, "")) {
			names = append(names, fmt.Sprintf("%d_%s", migration.Version, migration.Name))
		}
	}
	return names
}

// uuidv7Version is server_version_num of PostgreSQL 18, which added uuidv7().
const uuidv7Version = 180000

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
type dialect struct {
	// uuidv7 names the migrations that need PostgreSQL 18's uuidv7().
	uuidv7 []string
}

func (dialect) Placeholder(n int) string { return "$" + strconv.Itoa(n) }

func (d dialect) PrepareSession(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "SET statement_timeout = 0"); err != nil {
		return err
	}
	if len(d.uuidv7) == 0 {
		return nil
	}
	// Refused before anything is applied, instead of failing midway with
	// "function uuidv7() does not exist" (SQLSTATE 42883).
	var version int
	var name string
	if err := conn.QueryRowContext(ctx, "SELECT current_setting('server_version_num')::int, current_setting('server_version')").Scan(&version, &name); err != nil {
		return err
	}
	return requireUUIDv7(version, name, d.uuidv7)
}

// requireUUIDv7 fails when migrations call uuidv7() and the server, of
// server_version_num version, predates it.
func requireUUIDv7(version int, name string, migrations []string) error {
	if len(migrations) == 0 || version >= uuidv7Version {
		return nil
	}
	return fmt.Errorf("migrations %s call uuidv7(), which needs PostgreSQL 18 or later, and this server is PostgreSQL %s: "+
		"upgrade the server (Forge's identity migrations need PostgreSQL 18)", strings.Join(migrations, ", "), name)
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
