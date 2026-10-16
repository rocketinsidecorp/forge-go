package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/migrate"
)

// migrationLockName is the GET_LOCK name that serializes migrators across
// processes and replicas. User-level locks are server-wide and their names
// are limited to 64 characters, so the name carries a digest of the
// configured database: migrations of different databases on one server do not
// wait for each other. It is computed once from configuration, never from
// the session, which a migration could switch with USE.
func migrationLockName(database string) string {
	sum := sha256.Sum256([]byte(database))
	return "forge_schema_migrations:" + hex.EncodeToString(sum[:])[:32]
}

// NewMigrator returns a migrator for the database cfg points at. It opens its
// own dedicated connections, never ones from an application pool, with
// multiStatements enabled because a migration file holds many statements;
// close it when done.
//
// MySQL and MariaDB commit DDL implicitly: unlike PostgreSQL and SQLite, a
// migration that fails part-way leaves the statements before the failure
// applied, although the migration is not recorded. Keep each migration small,
// ideally one DDL statement, and write it so it can be completed by hand.
func NewMigrator(cfg config.Database, migrations []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
	driverConfig, err := newDriverConfig(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := openPool(cfg, true)
	if err != nil {
		return nil, err
	}
	migrator, err := migrate.New(pool, dialect{lockName: migrationLockName(driverConfig.DBName)}, migrations, logger)
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	return migrator, nil
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

// dialect is MySQL and MariaDB for the migration engine.
type dialect struct {
	lockName string
}

func (dialect) Placeholder(int) string { return "?" }

// PrepareSession lifts the statement timeout: index builds and backfills may
// legitimately run long.
func (dialect) PrepareSession(ctx context.Context, conn *sql.Conn) error {
	var version string
	if err := conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, "SET SESSION "+timeoutAssignment(detectFlavor(version), 0))
	return err
}

// TryLock takes the migration lock without waiting (timeout 0), as
// migrate.Dialect requires. The lock belongs to the session, so closing the
// session releases it even if Unlock never runs.
func (d dialect) TryLock(ctx context.Context, conn *sql.Conn) (bool, error) {
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", d.lockName).Scan(&acquired); err != nil {
		return false, err
	}
	if !acquired.Valid {
		return false, errors.New("GET_LOCK failed")
	}
	return acquired.Int64 == 1, nil
}

func (d dialect) Unlock(ctx context.Context, conn *sql.Conn) error {
	var released sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", d.lockName).Scan(&released); err != nil {
		return err
	}
	if !released.Valid || released.Int64 != 1 {
		return errors.New("the migration lock was not held by this session")
	}
	return nil
}

// SessionIdentity is the current database, which USE would change.
func (dialect) SessionIdentity(ctx context.Context, q migrate.Queryer) (string, error) {
	var database sql.NullString
	err := q.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database)
	return database.String, err
}

func (dialect) TableExists(ctx context.Context, conn *sql.Conn, table string) (bool, error) {
	var count int
	err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table).Scan(&count)
	return count > 0, err
}
