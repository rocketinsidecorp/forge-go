package postgres

import (
	"context"
	"embed"
	"io/fs"

	"github.com/rocketinsidecorp/forge-go/command"
	"github.com/rocketinsidecorp/forge-go/config"
	forgepostgres "github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/queue/queuecmd"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations returns the PostgreSQL job queue migrations rooted at ".".
// Compose them with the application's own through
// postgres.CommandsFromSets(app.Migrations(), queuepostgres.Migrations()).
func Migrations() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// Commands returns the "jobs" operator command (see queuecmd).
func Commands() []command.Command {
	return queuecmd.Commands(func(ctx context.Context, cfg config.Database) (queue.Admin, func() error, error) {
		db, err := forgepostgres.Open(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		store, err := NewStore(db)
		if err != nil {
			db.Close()
			return nil, nil, err
		}
		return store, func() error { db.Close(); return nil }, nil
	})
}
