package mysql

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth/internal/sqlidentity"
	"github.com/rocketinsidecorp/forge-go/fault"
	forgemysql "github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

//go:embed migrations/*.sql
var migrations embed.FS

// setupLockTimeout is how long setup waits for a concurrent setup, in
// seconds (GET_LOCK).
var setupLockTimeout = 10

// dialect is MySQL's and MariaDB's. The database clock decides every expiry
// (UTC_TIMESTAMP), so replicas with skewed clocks agree, as with
// PostgreSQL. Read-write transactions run READ COMMITTED, which takes no gap
// locks, and are retried after a deadlock; locking reads use FOR UPDATE, and
// the pruning batches FOR UPDATE SKIP LOCKED (MySQL 8.0 and MariaDB 10.6 and
// later).
var dialect = &sqlidentity.Dialect{
	Name:       "mysql",
	Now:        "UTC_TIMESTAMP(6)",
	Ago:        "DATE_SUB(UTC_TIMESTAMP(6), INTERVAL ? MICROSECOND)",
	AgoArg:     func(d time.Duration) any { return d.Microseconds() },
	Time:       func(t time.Time) any { return t.UTC() },
	Lock:       " FOR UPDATE",
	SkipLocked: " FOR UPDATE SKIP LOCKED",
	Tx:         sqldb.TxOptions{Isolation: sql.LevelReadCommitted, Attempts: 3},
	InsertIgnore: func(table string, columns, key []string, rows int) string {
		row := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ") + ")"
		// A no-op assignment, not INSERT IGNORE, which would also turn
		// other errors (a CHECK, a truncation) into warnings.
		return "INSERT INTO " + table + " (" + strings.Join(columns, ", ") + ") VALUES " +
			strings.TrimSuffix(strings.Repeat(row+", ", rows), ", ") +
			" ON DUPLICATE KEY UPDATE " + key[0] + " = " + key[0]
	},
	SetupLock: setupLock,
	IsUniqueViolation: func(err error) bool {
		translated := fault.From(forgemysql.Translate(err))
		return translated != nil && translated.Code == "already_exists"
	},
}

// setupLock runs fn in a transaction on one connection that holds the named
// lock name (GET_LOCK) from before the transaction begins until after it
// ends, so a concurrent setup starts its transaction only once this one
// committed and then sees its tenant.
func setupLock(ctx context.Context, db *sqldb.DB, name string, opts sqldb.TxOptions, fn func(*sql.Tx) error) (err error) {
	conn, err := db.SQL().Conn(ctx)
	if err != nil {
		return db.Translate(err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, setupLockTimeout).Scan(&acquired); err != nil {
		return db.Translate(err)
	}
	if acquired.Int64 != 1 {
		return fmt.Errorf("another setup holds the %s lock; try again", name)
	}
	defer func() {
		var released sql.NullInt64
		releaseErr := conn.QueryRowContext(context.WithoutCancel(ctx), "SELECT RELEASE_LOCK(?)", name).Scan(&released)
		err = errors.Join(err, db.Translate(releaseErr))
	}()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: opts.Isolation})
	if err != nil {
		return db.Translate(err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return db.Translate(tx.Commit())
}
