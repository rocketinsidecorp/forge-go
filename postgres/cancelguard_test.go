package postgres_test

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

// stallingConn reproduces, without a sleep, the interleaving that poisons a
// pgx connection: the writing goroutine is held back after its write reached
// the server (as the scheduler can hold it), pgx's slow-write timer (15 ms)
// starts the background reader, which reads the whole result and waits for
// more, and the query's context is cancelled at that moment.
type stallingConn struct {
	net.Conn
	scenario atomic.Pointer[stallScenario]
}

// stallScenario is one armed reproduction, played on the next write.
type stallScenario struct {
	cancel      context.CancelFunc
	armed       atomic.Bool
	readStarted chan struct{}
	readFailed  chan struct{}
	failOnce    sync.Once
}

func (c *stallingConn) Read(p []byte) (int, error) {
	s := c.scenario.Load()
	if s != nil {
		select {
		case s.readStarted <- struct{}{}:
		default:
		}
	}
	n, err := c.Conn.Read(p)
	if s != nil && err != nil {
		s.failOnce.Do(func() { close(s.readFailed) })
	}
	return n, err
}

func (c *stallingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	s := c.scenario.Load()
	if s == nil || !s.armed.CompareAndSwap(true, false) {
		return n, err
	}
	// The background reader's first read returns the whole result; its
	// second waits for bytes that are not coming.
	for range 2 {
		await(s.readStarted)
	}
	// The cancellation's deadline fails that pending read; pgx stores the
	// i/o timeout for the next read on the connection.
	s.cancel()
	await(s.readFailed)
	return n, err
}

// await waits for ch, bounded so a pgx change that removes the background
// reader makes the test fail instead of hang.
func await(ch <-chan struct{}) {
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
	}
}

// poisonOnce runs one query whose cancellation interleaves as above, on a
// one-connection pool, then a query whose context is never cancelled. It
// returns the cancelled query's error, the second query's error and whether
// the second ran on the same server process.
func poisonOnce(t *testing.T, guard bool) (cancelledErr, nextErr error, sameBackend bool) {
	t.Helper()
	ctx := context.Background()
	poolConfig, err := pgxpool.ParseConfig(postgrestest.Config(t).URL.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.MaxConns, poolConfig.MinConns = 1, 0
	// The reproduction counts the socket reads: no TLS layer in between.
	poolConfig.ConnConfig.TLSConfig, poolConfig.ConnConfig.Fallbacks = nil, nil
	var latest atomic.Pointer[stallingConn]
	poolConfig.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := new(net.Dialer).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		stalling := &stallingConn{Conn: conn}
		latest.Store(stalling)
		return stalling, nil
	}
	if guard {
		postgres.GuardCancelledConnections(poolConfig)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pid := conn.Conn().PgConn().PID()
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &stallScenario{cancel: cancel, readStarted: make(chan struct{}, 8), readFailed: make(chan struct{})}
	s.armed.Store(true)
	latest.Load().scenario.Store(s)
	// One write, one result: the low-level simple-protocol path.
	_, cancelledErr = conn.Conn().PgConn().Exec(queryCtx, "SELECT 1").ReadAll()
	latest.Load().scenario.Store(nil)
	conn.Release()

	next, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release()
	sameBackend = next.Conn().PgConn().PID() == pid
	// A query with an argument: the extended protocol reports the stored
	// error. (The simple protocol's first read ignores it and reads again.)
	var one int
	nextErr = next.QueryRow(ctx, "SELECT $1::int", 1).Scan(&one)
	return cancelledErr, nextErr, sameBackend
}

// Without the guard, pgx returns the poisoned connection to the pool and the
// next query fails although nothing cancelled it. This is the control: if it
// stops failing, pgx no longer stores the error and the guard can be
// revisited.
func TestCancelledQueryPoisonsUnguardedPool(t *testing.T) {
	t.Parallel()
	cancelledErr, nextErr, sameBackend := poisonOnce(t, false)
	if cancelledErr != nil {
		t.Fatalf("the cancelled query had its whole result and should succeed: %v", cancelledErr)
	}
	if !sameBackend || nextErr == nil || !isTimeout(nextErr) {
		t.Fatalf("pgx no longer poisons the connection (same backend %v, next query error %v): revisit guardCancelledConnections", sameBackend, nextErr)
	}
}

// With the guard, the connection the cancellation fired on is destroyed
// before reuse, and the next query runs on a new one.
func TestGuardDestroysConnectionACancellationFiredOn(t *testing.T) {
	t.Parallel()
	cancelledErr, nextErr, sameBackend := poisonOnce(t, true)
	if cancelledErr != nil {
		t.Fatalf("the cancelled query had its whole result and should succeed: %v", cancelledErr)
	}
	if sameBackend {
		t.Error("the pool reused the connection a cancellation fired on")
	}
	if nextErr != nil {
		t.Errorf("the next query failed: %v", nextErr)
	}
}

// The pool Open returns installs the guard: a connection is marked when a
// cancellation fires on it and destroyed instead of reused; an unmarked
// connection is reused.
func TestOpenGuardsCancelledConnections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := postgrestest.Config(t)
	cfg.MaxConns, cfg.MinConns = 1, 0
	db, err := postgres.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pool := db.Pool()

	pid := func() uint32 {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		return conn.Conn().PgConn().PID()
	}
	first := pid()
	if again := pid(); again != first {
		t.Fatalf("an unmarked connection was not reused: backend %d, then %d", first, again)
	}

	// A cancellation that fires during a query marks the connection.
	observer, err := pgx.Connect(ctx, cfg.URL.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = observer.Close(ctx) }()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	backend := int64(conn.Conn().PgConn().PID())
	failed := make(chan error, 1)
	go func() {
		_, err := conn.Exec(queryCtx, "SELECT pg_sleep(30)")
		failed <- err
	}()
	waittest.Until(t, 10*time.Second, "the query to run", func() bool {
		var running bool
		err := observer.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pg_stat_activity WHERE pid = $1 AND state = 'active' AND query LIKE '%pg_sleep%')",
			backend).Scan(&running)
		return err == nil && running
	})
	cancel()
	if err := <-failed; err == nil {
		t.Fatal("the cancelled query succeeded")
	}
	if !postgres.Cancelled(conn.Conn()) {
		t.Error("the cancellation did not mark the connection")
	}
	conn.Release()

	// A marked connection that is otherwise healthy is destroyed too.
	conn, err = pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	healthy := conn.Conn().PgConn().PID()
	if !postgres.MarkCancelled(conn.Conn()) {
		t.Fatal("Open's pool does not carry the cancellation mark")
	}
	conn.Release()
	if next := pid(); next == healthy {
		t.Error("the pool reused a marked connection")
	}
	if created := pool.Stat().NewConnsCount(); created != 3 {
		t.Errorf("%d connections created, want 3 (first, after the cancelled query, after the marked one)", created)
	}
}
