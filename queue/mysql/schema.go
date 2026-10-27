package mysql

import (
	"context"
	"embed"
	"io/fs"

	"github.com/rocketinsidecorp/forge-go/command"
	"github.com/rocketinsidecorp/forge-go/config"
	forgemysql "github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/queue/internal/jobscmd"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations returns the MySQL and MariaDB job queue migrations rooted at
// ".". Compose them with the application's own through
// mysql.CommandsFromSets(app.Migrations(), queuemysql.Migrations()).
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
	return jobscmd.Commands(func(ctx context.Context, cfg config.Database) (queue.Admin, func() error, error) {
		db, err := forgemysql.Open(ctx, cfg)
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
