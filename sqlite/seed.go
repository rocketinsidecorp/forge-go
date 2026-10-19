package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	forge "github.com/rocketinsidecorp/forge-go"
)

// SeedFunc loads the rows every environment of an application needs, such as
// lookup tables and default settings, inside the transaction SeedCommand
// opens. It must be idempotent (insert only rows that are missing), because
// seeding is meant to be run again, for example after every deploy.
type SeedFunc func(ctx context.Context, tx *sql.Tx) error

// SeedCommand returns the "seed" subcommand: it runs seed in one transaction
// on the configured database and commits only when seed returns nil, so a
// failed seed changes nothing. Generated applications register it with their
// db.Seed function.
func SeedCommand(seed SeedFunc) forge.Command {
	return forge.Command{
		Name:    "seed",
		Summary: "load the application's seed data in one transaction (idempotent; run after migrate)",
		Run: func(ctx context.Context, env forge.CommandEnv, args []string) error {
			if seed == nil {
				return errors.New("no seed function is registered")
			}
			if len(args) != 0 {
				return fmt.Errorf("%w: seed accepts no arguments", forge.ErrUsage)
			}
			database, err := Open(ctx, env.Config.Database)
			if err != nil {
				return err
			}
			if err := database.InTx(ctx, func(tx *sql.Tx) error { return seed(ctx, tx) }); err != nil {
				return errors.Join(err, database.Close())
			}
			if err := database.Close(); err != nil {
				return err
			}
			_, err = fmt.Fprintln(env.Stdout, "seed data loaded")
			return err
		},
	}
}
