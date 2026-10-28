package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth/authsql"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/sqldb"
	forgesqlite "github.com/rocketinsidecorp/forge-go/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// now is SQLite's current UTC time in authsql.TimeFormat (millisecond
// precision, padded to six digits).
const now = `strftime('%Y-%m-%d %H:%M:%f000', 'now')`

// dialect is SQLite's. Every write transaction starts with BEGIN IMMEDIATE
// (forge-go/sqlite), which takes the database's single write lock: locking
// reads need no FOR UPDATE, nothing is ever skipped as locked, and setup is
// serialized by the transaction itself. The application's processes share
// one host (ADR 0009), so the system clock SQLite reads is theirs too.
var dialect = &authsql.Dialect{
	Name: "sqlite",
	Now:  now,
	Ago:  `strftime('%Y-%m-%d %H:%M:%f000', 'now', ?)`,
	AgoArg: func(d time.Duration) any {
		return fmt.Sprintf("-%d.%06d seconds", d/time.Second, (d%time.Second)/time.Microsecond)
	},
	Time: func(t time.Time) any { return t.UTC().Format(authsql.TimeFormat) },
	InsertIgnore: func(table string, columns, key []string, rows int) string {
		row := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ") + ")"
		return "INSERT INTO " + table + " (" + strings.Join(columns, ", ") + ") VALUES " +
			strings.TrimSuffix(strings.Repeat(row+", ", rows), ", ") +
			" ON CONFLICT (" + strings.Join(key, ", ") + ") DO NOTHING"
	},
	SetupLock: func(ctx context.Context, db *sqldb.DB, _ string, opts sqldb.TxOptions, fn func(*sql.Tx) error) error {
		return db.InTxWith(ctx, opts, fn)
	},
	IsUniqueViolation: func(err error) bool {
		translated := fault.From(forgesqlite.Translate(err))
		return translated != nil && translated.Code == "already_exists"
	},
}
