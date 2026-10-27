package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/command"
)

// SeedFunc loads the rows every environment of an application needs, such as
// lookup tables and default settings, inside the transaction SeedCommand
// opens. It must be idempotent (INSERT ... ON CONFLICT DO NOTHING, or check
// before writing), because seeding is meant to be run again, for example
// after every deploy. Tenant-scoped tables behind row-level security need
// SELECT set_config('forge.tenant_id', $1, true) before their rows.
type SeedFunc func(ctx context.Context, tx pgx.Tx) error

// SeedCommand returns the "seed" subcommand: it runs seed in one READ
// COMMITTED transaction on the configured database and commits only when
// seed returns nil, so a failed seed changes nothing. Generated applications
// register it with their db.Seed function.
func SeedCommand(seed SeedFunc) command.Command {
	return command.Command{
		Name:    "seed",
		Summary: "load the application's seed data in one transaction (idempotent; run after migrate)",
		Run: func(ctx context.Context, env command.Env, args []string) error {
			if seed == nil {
				return errors.New("no seed function is registered")
			}
			if len(args) != 0 {
				return fmt.Errorf("%w: seed accepts no arguments", command.ErrUsage)
			}
			database, err := Open(ctx, env.Config.Database)
			if err != nil {
				return err
			}
			defer database.Close()
			if err := database.InTx(ctx, func(tx pgx.Tx) error { return seed(ctx, tx) }); err != nil {
				return err
			}
			_, err = fmt.Fprintln(env.Stdout, "seed data loaded")
			return err
		},
	}
}
