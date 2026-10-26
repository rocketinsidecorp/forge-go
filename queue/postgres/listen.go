package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/queue"
)

// closeTimeout bounds closing the listening connection, so a dead network
// cannot hold up a worker's shutdown.
const closeTimeout = time.Second

// Listen implements queue.Listener for a store built WithNotify: it holds one
// connection LISTENing on NotifyChannel until ctx ends or the connection
// fails, calling wake with the queue named by each notification. Without
// WithNotify it returns queue.ErrListenUnsupported at once. A Worker calls it
// on its own goroutine and reconnects with backoff; applications do not call
// it.
//
// The connection is taken from the pool and then detached from it (it is
// never returned while subscribed), so it does not count against the pool's
// MaxConns; it is closed when Listen returns.
func (s *Store) Listen(ctx context.Context, listening func(), wake func(queueName string)) error {
	if !s.notify {
		return queue.ErrListenUnsupported
	}
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire job listener connection: %w", err)
	}
	// Hijack so the connection leaves the pool and is closed, rather than
	// returning to it still LISTENing.
	conn := pooled.Hijack()
	defer closeConn(conn)
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{NotifyChannel}.Sanitize()); err != nil {
		return fmt.Errorf("listen for jobs: %w", err)
	}
	listening()
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait for job notification: %w", err)
		}
		// The channel is writable by any role on the database: ignore
		// anything that is not a queue name.
		if notification.Channel == NotifyChannel && queue.ValidateName(notification.Payload) == nil {
			wake(notification.Payload)
		}
	}
}

func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_ = conn.Close(ctx)
}
