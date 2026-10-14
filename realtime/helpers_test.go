package realtime_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/fabriciobonjorno/forge-go/auth"
	"github.com/fabriciobonjorno/forge-go/realtime"
	"github.com/fabriciobonjorno/forge-go/tenancy"
	"github.com/fabriciobonjorno/forge-go/uuid"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// directory is an in-memory session store: tokens resolve to principals, so
// tests authenticate through the real auth middleware.
type directory struct {
	mu       sync.Mutex
	sessions map[auth.Digest]auth.Principal
}

func newDirectory() *directory {
	return &directory{sessions: map[auth.Digest]auth.Principal{}}
}

func (d *directory) middleware(t testing.TB) *auth.Middleware {
	t.Helper()
	m, err := auth.NewMiddleware(auth.ResolverFunc(func(_ context.Context, digest auth.Digest) (auth.Session, bool, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		principal, ok := d.sessions[digest]
		return auth.Session{Principal: principal, ExpiresAt: time.Now().Add(time.Hour)}, ok, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// user is a signed-in test subject.
type user struct {
	token     string
	principal auth.Principal
	tenant    tenancy.Tenant
}

func (d *directory) add(t testing.TB, tenant tenancy.Tenant, permissions ...string) user {
	t.Helper()
	granted := make([]auth.Permission, len(permissions))
	for i, p := range permissions {
		granted[i] = auth.Permission(p)
	}
	principal, err := auth.NewPrincipal(uuid.MustNew(), tenant, granted...)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.sessions[token.Digest()] = principal
	d.mu.Unlock()
	return user{token: token.Reveal(), principal: principal, tenant: tenant}
}

func newTenant(t testing.TB) tenancy.Tenant {
	t.Helper()
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	return tenant
}

// context returns a request context authenticated as u, obtained from the
// real middleware.
func (d *directory) context(t testing.TB, u user) context.Context {
	t.Helper()
	var ctx context.Context
	handler := d.middleware(t).Authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { ctx = r.Context() }))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+u.token)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if ctx == nil {
		t.Fatal("authentication failed")
	}
	return ctx
}

func newBroker(t testing.TB, options realtime.Options) *realtime.Broker {
	t.Helper()
	if options.Logger == nil {
		options.Logger = discard
	}
	ctx, cancel := context.WithCancel(context.Background())
	broker, err := realtime.New(ctx, options)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := broker.Close(closeCtx); err != nil {
			t.Errorf("close broker: %v", err)
		}
		cancel()
	})
	return broker
}

// connect opens a connection for u subscribed to topics.
func connect(t testing.TB, broker *realtime.Broker, d *directory, u user, topics ...string) *realtime.Conn {
	t.Helper()
	ctx := d.context(t, u)
	conn, err := broker.NewConn(ctx)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	t.Cleanup(conn.Close)
	for _, topic := range topics {
		if err := conn.Subscribe(ctx, topic); err != nil {
			t.Fatalf("Subscribe(%q): %v", topic, err)
		}
	}
	return conn
}

// next returns the next non-synthetic event, failing after a timeout.
func next(t testing.TB, conn *realtime.Conn) realtime.Event {
	t.Helper()
	return nextMatching(t, conn, func(e realtime.Event) bool { return !e.Synthetic() })
}

func nextMatching(t testing.TB, conn *realtime.Conn, match func(realtime.Event) bool) realtime.Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-conn.Events():
			if match(event) {
				return event
			}
		case <-conn.Done():
			t.Fatalf("connection closed while waiting for an event: %v", conn.Err())
		case <-timeout:
			t.Fatal("timed out waiting for an event")
		}
	}
}

// expectNone fails if an application event arrives within the window.
func expectNone(t testing.TB, conn *realtime.Conn, wait time.Duration) {
	t.Helper()
	timer := time.After(wait)
	for {
		select {
		case event := <-conn.Events():
			if !event.Synthetic() {
				t.Fatalf("unexpected event %s on %q", event.Type, event.Topic)
			}
		case <-timer:
			return
		}
	}
}

// settleGoroutines waits for the goroutine count to fall back to want or
// below, which is how the leak tests detect stragglers.
func settleGoroutines(t testing.TB, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		if got := runtime.NumGoroutine(); got <= want {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutines did not return to %d: now %d\n%s", want, runtime.NumGoroutine(), buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
