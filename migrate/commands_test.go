package migrate

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"slices"
	"testing"
	"testing/fstest"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
)

func TestWithMigratorRejectsSetCollisionBeforeOpeningDatabase(t *testing.T) {
	first := fstest.MapFS{
		"20260101000000_framework.up.sql": {Data: []byte("SELECT 1;")},
	}
	second := fstest.MapFS{
		"20260101000000_application.up.sql": {Data: []byte("SELECT 2;")},
	}
	called := false
	open := Opener(func(context.Context, config.Database, []Migration, *slog.Logger) (*Migrator, error) {
		called = true
		t.Fatal("database opener called before migration sets were validated")
		return nil, nil
	})

	err := withMigrator(
		context.Background(),
		forge.CommandEnv{},
		[]fs.FS{first, second},
		open,
		func(*Migrator) error { return nil },
	)
	if err == nil {
		t.Fatal("expected cross-set version collision")
	}
	if called {
		t.Fatal("database opener was called")
	}
}

func TestWithMigratorConnectsAsTheMigrationRole(t *testing.T) {
	set := fstest.MapFS{"20260101000000_a.up.sql": {Data: []byte("SELECT 1;")}}
	served := "postgres://app_runtime:rt@db:5432/shop?sslmode=disable"
	var opened []string
	open := Opener(func(_ context.Context, cfg config.Database, _ []Migration, _ *slog.Logger) (*Migrator, error) {
		opened = append(opened, cfg.URL.Reveal())
		return nil, errors.New("stop")
	})
	env := forge.CommandEnv{Config: config.Default()}
	env.Config.Database.URL = config.NewSecret(served)

	// Without LookupEnv (or the variable) the serving URL is used.
	_ = withMigrator(context.Background(), env, []fs.FS{set}, open, func(*Migrator) error { return nil })
	env.LookupEnv = func(key string) (string, bool) {
		if key == config.EnvMigrationDatabaseURL {
			return "postgres://app_migrator:m@db:5432/postgres?sslmode=disable", true
		}
		return "", false
	}
	_ = withMigrator(context.Background(), env, []fs.FS{set}, open, func(*Migrator) error { return nil })
	want := []string{served, "postgres://app_migrator:m@db:5432/shop?sslmode=disable"}
	if !slices.Equal(opened, want) {
		t.Fatalf("opened %q, want %q", opened, want)
	}

	env.LookupEnv = func(key string) (string, bool) { return "mysql://m:pw@db/shop", key == config.EnvMigrationDatabaseURL }
	if err := withMigrator(context.Background(), env, []fs.FS{set}, open, func(*Migrator) error { return nil }); err == nil || len(opened) != 2 {
		t.Fatalf("an invalid migration URL: err = %v, opened %d times; want a refusal before opening", err, len(opened))
	}
}
