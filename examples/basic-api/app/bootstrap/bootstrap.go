// Package bootstrap is the composition root: it acquires infrastructure and
// wires repositories, use cases and handlers into a forge.App.
package bootstrap

import (
	"context"
	"fmt"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/adapters/httpapi"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/adapters/persistence"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/application"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// Configure implements forge.Configure. The service needs its database:
// startup fails when FORGE_DATABASE_URL is unset or unreachable.
func Configure(ctx context.Context, app *forge.App) error {
	database, err := postgres.Open(ctx, app.Config().Database)
	if err != nil {
		return err
	}
	if err := app.OnShutdown(database.Shutdown); err != nil {
		database.Close()
		return err
	}
	if err := app.Readiness("database", database.Ping); err != nil {
		return err
	}

	tasks, err := application.NewService(
		persistence.NewTaskRepository(database),
		persistence.NewTransactor(database),
		time.Now,
		uuid.New,
	)
	if err != nil {
		return err
	}
	if err := httpapi.Register(app, tasks); err != nil {
		return fmt.Errorf("register task routes: %w", err)
	}
	return nil
}
