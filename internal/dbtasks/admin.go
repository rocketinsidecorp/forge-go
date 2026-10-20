package dbtasks

import (
	"context"

	"github.com/rocketinsidecorp/forge-go/config"
)

// Exists reports whether the target database exists.
func (t *Target) Exists(ctx context.Context) (bool, error) {
	switch t.Adapter {
	case config.AdapterPostgres:
		return t.pgExists(ctx)
	case config.AdapterMySQL:
		return t.mysqlExists(ctx)
	default:
		return t.sqliteExists()
	}
}

// Create creates the database. It reports false, without error, when it
// already exists.
func (t *Target) Create(ctx context.Context) (bool, error) {
	if err := t.checkMutable(); err != nil {
		return false, err
	}
	switch t.Adapter {
	case config.AdapterPostgres:
		return t.pgCreate(ctx)
	case config.AdapterMySQL:
		return t.mysqlCreate(ctx)
	default:
		return t.sqliteCreate()
	}
}

// Drop removes the database and everything in it. It reports false, without
// error, when there was none. Callers must have passed the destructive-action
// guard first; this method does not ask.
func (t *Target) Drop(ctx context.Context) (bool, error) {
	if err := t.checkMutable(); err != nil {
		return false, err
	}
	switch t.Adapter {
	case config.AdapterPostgres:
		return t.pgDrop(ctx)
	case config.AdapterMySQL:
		return t.mysqlDrop(ctx)
	default:
		return t.sqliteDrop()
	}
}
