// Package authsql is the database/sql identity store that auth/sqlite and
// auth/mysql share: the same queries as auth/postgres, written once for the
// SQL both databases accept, with the few constructs they spell differently
// (the clock, locking reads, upserts, the setup lock) supplied by a Dialect.
//
// It mirrors auth/postgres exactly: the same tables, the same rules (an
// inactive user, membership, tenant or organization signs nobody in; a
// credential version change revokes every session and MFA challenge), the
// same lock order (the user row first) and the same results. The shared
// contract suite auth/authtest runs against all three stores.
//
// Queries carry three placeholders that the dialect expands once: {now} is
// the database's current UTC time, {ago} the current time minus the duration
// bound to its one parameter (Dialect.AgoArg), and {lock} the suffix of a
// locking read.
//
// It is public because auth/sqlite and auth/mysql depend on it and are to
// live in their own Go modules, which may not import another module's
// internal packages (ADR 0053). Applications use those stores, not this
// package; its API follows them and carries no compatibility promise of its
// own beyond theirs.
package authsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// Dialect is what differs between the databases the store runs on.
type Dialect struct {
	// Name is "sqlite" or "mysql", for messages.
	Name string
	// Now is an SQL expression for the current UTC time, comparable with
	// stored timestamps.
	Now string
	// Ago is an SQL expression with one placeholder: the current UTC time
	// minus the duration AgoArg turns into that parameter.
	Ago    string
	AgoArg func(time.Duration) any
	// Time is the parameter a timestamp column stores t as.
	Time func(t time.Time) any
	// Lock is the suffix of a read that locks the rows it returns until the
	// transaction ends (" FOR UPDATE"), empty where the transaction itself
	// holds the database's write lock (SQLite's BEGIN IMMEDIATE).
	Lock string
	// SkipLocked is the suffix of a locking read that skips rows another
	// transaction holds, for the pruning batches.
	SkipLocked string
	// Tx configures read-write transactions (isolation, deadlock retries).
	Tx sqldb.TxOptions
	// InsertIgnore returns an INSERT of rows rows of len(columns) values
	// into table that leaves an existing row with the same key (the
	// columns in key) unchanged.
	InsertIgnore func(table string, columns, key []string, rows int) string
	// SetupLock runs fn in a transaction that no other setup runs
	// concurrently with, on any process.
	SetupLock func(ctx context.Context, db *sqldb.DB, name string, opts sqldb.TxOptions, fn func(*sql.Tx) error) error
	// IsUniqueViolation reports a duplicate key error.
	IsUniqueViolation func(error) bool
}

func (d *Dialect) validate() error {
	if d == nil || d.Name == "" || d.Now == "" || d.Ago == "" || d.AgoArg == nil || d.Time == nil ||
		d.InsertIgnore == nil || d.SetupLock == nil || d.IsUniqueViolation == nil {
		return errors.New("identity store dialect is incomplete")
	}
	return nil
}

// TimeFormat is SQLite's stored timestamp: fixed-width UTC text, so TEXT
// columns compare and sort chronologically (the format of queue/sqlite).
const TimeFormat = "2006-01-02 15:04:05.000000"

// timestamp scans a timestamp column: a time.Time (MySQL) or TimeFormat text
// (SQLite).
type timestamp struct{ Time time.Time }

// Scan implements sql.Scanner.
func (t *timestamp) Scan(value any) error {
	switch v := value.(type) {
	case time.Time:
		t.Time = v.UTC()
		return nil
	case string:
		return t.parse(v)
	case []byte:
		return t.parse(string(v))
	default:
		return fmt.Errorf("identity store: cannot scan %T as a timestamp", value)
	}
}

func (t *timestamp) parse(text string) error {
	parsed, err := time.ParseInLocation(TimeFormat, text, time.UTC)
	if err != nil {
		return fmt.Errorf("identity store: invalid stored timestamp %q: %w", text, err)
	}
	t.Time = parsed
	return nil
}

// db is the part of *sqldb.DB and *sql.Tx a statement needs.
type db interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
