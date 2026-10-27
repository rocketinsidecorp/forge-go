package postgres

import (
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// GuardCancelledConnections exposes the pool guard Open installs, for tests
// that build a pool with a test-only dialer.
var GuardCancelledConnections = guardCancelledConnections

// MarkCancelled marks conn as the guard's context watcher does when a
// cancellation fires, and reports whether conn carried a mark to set.
func MarkCancelled(conn *pgx.Conn) bool {
	mark, ok := conn.PgConn().CustomData()[cancelledKey].(*atomic.Bool)
	if ok {
		mark.Store(true)
	}
	return ok
}

// Cancelled reports whether the guard marked conn.
var Cancelled = cancelled
