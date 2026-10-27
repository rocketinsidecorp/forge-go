package sqlite

import (
	"context"
	"embed"
	"io/fs"

	"github.com/rocketinsidecorp/forge-go/command"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/queue/queuecmd"
	forgesqlite "github.com/rocketinsidecorp/forge-go/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations returns the SQLite job queue migrations rooted at ".". Compose
// them with the application's own through
// sqlite.CommandsFromSets(app.Migrations(), queuesqlite.Migrations()).
func Migrations() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// Commands returns the "jobs" operator command (see queuecmd) backed by
// this store.
func Commands() []command.Command {
	return queuecmd.Commands(func(ctx context.Context, cfg config.Database) (queue.Admin, func() error, error) {
		db, err := forgesqlite.Open(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		store, err := NewStore(db)
		if err != nil {
			_ = db.Close()
			return nil, nil, err
		}
		return store, db.Close, nil
	})
}
