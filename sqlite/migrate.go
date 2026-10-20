package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"log/slog"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/migrate"
)

// NewMigrator returns a migrator for the database cfg points at, creating the
// file when missing. It opens its own connections, with the same settings as
// Open, never ones from an application pool; close it when done.
//
// Migrations run in immediate transactions, and SQLite's DDL is
// transactional: a failed migration leaves no trace. A file may hold many
// statements. Foreign keys are enforced during migrations, and SQLite
// ignores PRAGMA foreign_keys inside a transaction, so a table rebuild (its
// way to change a column) of a table other tables reference runs outside
// the engine's transaction, following https://www.sqlite.org/lang_altertable.html:
//
//	-- forge:no-transaction
//	PRAGMA foreign_keys = OFF;
//	BEGIN;
//	CREATE TABLE tasks_new (...);
//	INSERT INTO tasks_new SELECT ... FROM tasks;
//	DROP TABLE tasks;
//	ALTER TABLE tasks_new RENAME TO tasks;
//	COMMIT;
//	PRAGMA foreign_keys = ON;
//
// If a statement fails, the migration session is closed, which rolls the
// rebuild back.
func NewMigrator(cfg config.Database, migrations []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
	path, err := prepareFile(cfg)
	if err != nil {
		return nil, err
	}
	db, err := openDB(path, cfg.ConnectTimeout, nil)
	if err != nil {
		return nil, err
	}
	migrator, err := migrate.New(db, &dialect{lock: newMigrationLock(path)}, migrations, logger)
	if err != nil {
		_ = db.Close()
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

// dialect is SQLite for the migration engine.
type dialect struct {
	lock *migrationLock
}

func (*dialect) Placeholder(int) string { return "?" }

// PrepareSession has nothing to lift: SQLite has no statement timeout.
func (*dialect) PrepareSession(context.Context, *sql.Conn) error { return nil }

func (d *dialect) TryLock(ctx context.Context, conn *sql.Conn) (bool, error) {
	return d.lock.tryLock(ctx, conn)
}

func (d *dialect) Unlock(_ context.Context, conn *sql.Conn) error { return d.lock.unlock(conn) }

func (*dialect) TableExists(ctx context.Context, conn *sql.Conn, table string) (bool, error) {
	var exists bool
	err := conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)", table).Scan(&exists)
	return exists, err
}
