package migrate

import (
	"context"
	"io/fs"
	"log/slog"
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
