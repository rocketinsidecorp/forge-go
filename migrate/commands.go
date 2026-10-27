package migrate

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/rocketinsidecorp/forge-go/command"
	"github.com/rocketinsidecorp/forge-go/config"
)

// Opener returns a migrator for the configured database; Commands closes it.
type Opener func(ctx context.Context, cfg config.Database, migrations []Migration, logger *slog.Logger) (*Migrator, error)

// Commands returns the database subcommands for an application binary:
//
//	app migrate           apply pending migrations
//	app migrate status    list migrations and their state
//	app rollback [-steps N]  revert application migrations only
//
// Because the migrations are embedded in the binary, the production image can
// migrate its own database (for example as a one-off container or a
// Kubernetes Job) with no extra tooling.
//
// They connect as the migration role of FORGE_MIGRATION_DATABASE_URL when it
// is set, to the database of FORGE_DATABASE_URL (see
// config.Config.MigrationDatabase), so the role the application serves with
// needs no DDL privileges.
func Commands(migrations fs.FS, open Opener) []command.Command {
	return CommandsFromSets([]fs.FS{migrations}, open)
}

// CommandsFromSets is Commands for several independent migration filesystems.
// Sets are validated and merged by version before a database connection opens.
// rollback reverts application migrations only (Migrator.Down): framework
// migrations composed here (FrameworkMigrations) stay applied, and a rollback
// with only framework migrations left fails without reverting anything.
func CommandsFromSets(migrationSets []fs.FS, open Opener) []command.Command {
	return []command.Command{
		{Name: "migrate", Summary: "apply pending database migrations; \"migrate status\" lists them", Run: func(ctx context.Context, env command.Env, args []string) error {
			switch {
			case len(args) == 0:
				return withMigrator(ctx, env, migrationSets, open, func(m *Migrator) error {
					applied, err := m.Up(ctx)
					for _, migration := range applied {
						fmt.Fprintf(env.Stdout, "applied  %d_%s\n", migration.Version, migration.Name)
					}
					if err == nil && len(applied) == 0 {
						fmt.Fprintln(env.Stdout, "database is up to date")
					}
					return err
				})
			case len(args) == 1 && args[0] == "status":
				return withMigrator(ctx, env, migrationSets, open, func(m *Migrator) error {
					statuses, err := m.Status(ctx)
					if err != nil {
						return err
					}
					for _, status := range statuses {
						appliedAt := ""
						if !status.AppliedAt.IsZero() {
							appliedAt = status.AppliedAt.UTC().Format(time.RFC3339)
						}
						fmt.Fprintf(env.Stdout, "%-9s %d_%s %s\n", status.State, status.Version, status.Name, appliedAt)
					}
					return nil
				})
			default:
				return fmt.Errorf("%w: migrate [status]", command.ErrUsage)
			}
		}},
		{Name: "rollback", Summary: "revert the most recent application migrations (-steps N, default 1); Forge framework migrations are never reverted", Run: func(ctx context.Context, env command.Env, args []string) error {
			flags := flag.NewFlagSet("rollback", flag.ContinueOnError)
			flags.SetOutput(env.Stderr)
			steps := flags.Int("steps", 1, "number of application migrations to revert")
			if err := flags.Parse(args); err != nil {
				return fmt.Errorf("%w: %w", command.ErrUsage, err)
			}
			if flags.NArg() != 0 || *steps < 1 {
				return fmt.Errorf("%w: rollback [-steps N] with N >= 1", command.ErrUsage)
			}
			return withMigrator(ctx, env, migrationSets, open, func(m *Migrator) error {
				reverted, err := m.Down(ctx, *steps)
				for _, migration := range reverted {
					fmt.Fprintf(env.Stdout, "reverted %d_%s\n", migration.Version, migration.Name)
				}
				return err
			})
		}},
	}
}

func withMigrator(ctx context.Context, env command.Env, migrationSets []fs.FS, open Opener, fn func(*Migrator) error) (err error) {
	// Validate every set and global version uniqueness before touching the database.
	migrations, err := LoadMigrationSets(migrationSets...)
	if err != nil {
		return err
	}
	if env.Config.Database.URL.IsZero() {
		return errors.New("database is not configured: set FORGE_DATABASE_URL")
	}
	lookup := env.LookupEnv
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	database, err := env.Config.MigrationDatabase(lookup)
	if err != nil {
		return err
	}
	migrator, err := open(ctx, database, migrations, env.Logger)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, migrator.Close()) }()
	return fn(migrator)
}
