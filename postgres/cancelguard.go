package postgres

import (
	"context"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cancelledKey is the pgconn CustomData key of a connection's cancellation
// mark, an *atomic.Bool.
const cancelledKey = "forge.postgres.cancelled"

// guardCancelledConnections makes the pool destroy, instead of reuse, every
// connection on which a context cancellation interrupted an operation.
//
// pgx interrupts a cancelled operation by setting a deadline on the socket.
// If pgx's background reader (started when a write takes over 15 ms) still
// has a read pending at that moment, the read fails with the deadline's
// i/o timeout, and pgx keeps that error for the next read: when the
// cancelled operation had already received its whole result, it succeeds,
// the connection returns to the pool looking healthy, and the next query on
// it, whatever its context, fails with "i/o timeout". The deadline itself is
// cleared; the stored error is not, and pgx exposes no way to drop it.
//
// Every socket deadline on a pooled connection comes from its context
// watcher, so a connection whose watcher never fired cannot hold such an
// error. The guard wraps the watcher handler pgx builds for the connection
// to mark the connection when it fires, and checks the mark before the
// pool hands the connection out (PrepareConn): a marked connection is
// destroyed and Acquire takes another. Queries whose context is not
// cancelled while they run never mark a connection, so ordinary load
// destroys nothing; a cancellation that fails its query closes the
// connection anyway. A handler or PrepareConn already configured still
// runs.
func guardCancelledConnections(poolConfig *pgxpool.Config) {
	build := poolConfig.ConnConfig.BuildContextWatcherHandler
	poolConfig.ConnConfig.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
		var inner ctxwatch.Handler
		if build != nil {
			inner = build(conn)
		} else {
			inner = &pgconn.DeadlineContextWatcherHandler{Conn: conn.Conn()}
		}
		mark := new(atomic.Bool)
		conn.CustomData()[cancelledKey] = mark
		return &markingHandler{inner: inner, cancelled: mark}
	}

	prepare := poolConfig.PrepareConn
	poolConfig.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		if cancelled(conn) {
			return false, nil
		}
		if prepare != nil {
			return prepare(ctx, conn)
		}
		return true, nil
	}
}

// cancelled reports whether a cancellation fired on conn's context watcher.
func cancelled(conn *pgx.Conn) bool {
	mark, ok := conn.PgConn().CustomData()[cancelledKey].(*atomic.Bool)
	return ok && mark.Load()
}

// markingHandler marks its connection when the watched context is
// cancelled, then lets pgx's handler interrupt the operation as before.
type markingHandler struct {
	inner     ctxwatch.Handler
	cancelled *atomic.Bool
}

// HandleCancel runs on the context's AfterFunc goroutine; the mark is
// atomic because PrepareConn reads it from the acquiring goroutine.
func (h *markingHandler) HandleCancel(ctx context.Context) {
	h.cancelled.Store(true)
	h.inner.HandleCancel(ctx)
}

// HandleUnwatchAfterCancel delegates unchanged: pgx clears the deadline.
func (h *markingHandler) HandleUnwatchAfterCancel() { h.inner.HandleUnwatchAfterCancel() }
