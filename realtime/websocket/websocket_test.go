package websocket_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	ws "github.com/coder/websocket"

	"github.com/fabriciobonjorno/forge-go/auth"
	"github.com/fabriciobonjorno/forge-go/realtime"
	"github.com/fabriciobonjorno/forge-go/realtime/websocket"
	"github.com/fabriciobonjorno/forge-go/tenancy"
	"github.com/fabriciobonjorno/forge-go/uuid"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type user struct {
	cookie    string
	principal auth.Principal
	tenant    tenancy.Tenant
}

type fixture struct {
	t        *testing.T
	mu       sync.Mutex
	sessions map[auth.Digest]auth.Principal
	broker   *realtime.Broker
	server   *httptest.Server
	tenant   tenancy.Tenant
}

func newFixture(t *testing.T, brokerOptions realtime.Options, options websocket.Options) *fixture {
	t.Helper()
	f := &fixture{t: t, sessions: map[auth.Digest]auth.Principal{}}
	middleware, err := auth.NewMiddleware(auth.ResolverFunc(func(_ context.Context, digest auth.Digest) (auth.Session, bool, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		principal, ok := f.sessions[digest]
		return auth.Session{Principal: principal, ExpiresAt: time.Now().Add(time.Hour)}, ok, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if brokerOptions.Channels == nil {
		brokerOptions.Channels = []realtime.Channel{
			{Name: "orders", Permission: "orders:read"},
			{Name: "room:{id}", Permission: "rooms:read"},
		}
	}
	brokerOptions.Logger = discard
	ctx, cancel := context.WithCancel(context.Background())
	f.broker, err = realtime.New(ctx, brokerOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = f.broker.Close(closeCtx)
		cancel()
	})
	handler, err := websocket.Handler(f.broker, options)
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(middleware.AuthenticateCookie(handler))
	t.Cleanup(f.server.Close)
	f.tenant = newTenant(t)
	return f
}

func newTenant(t testing.TB) tenancy.Tenant {
	t.Helper()
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	return tenant
}

func (f *fixture) user(tenant tenancy.Tenant, permissions ...string) user {
	f.t.Helper()
	granted := make([]auth.Permission, len(permissions))
	for i, p := range permissions {
		granted[i] = auth.Permission(p)
	}
	principal, err := auth.NewPrincipal(uuid.MustNew(), tenant, granted...)
	if err != nil {
		f.t.Fatal(err)
	}
	token, err := auth.NewToken()
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.sessions[token.Digest()] = principal
	f.mu.Unlock()
	cookie := &http.Cookie{Name: auth.SessionCookieName, Value: token.Reveal()}
	return user{cookie: cookie.String(), principal: principal, tenant: tenant}
}

// publish publishes as a job for the tenant, outside any request.
func (f *fixture) publish(tenant tenancy.Tenant, topic, event string, payload any) realtime.Event {
	f.t.Helper()
	e, err := f.broker.Publish(context.Background(), topic, event, payload, realtime.ForTenant(tenant))
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

func (f *fixture) wsURL() string { return "ws" + strings.TrimPrefix(f.server.URL, "http") + "/ws" }

func (f *fixture) dial(u user, origin string) (*ws.Conn, *http.Response, error) {
	f.t.Helper()
	header := http.Header{"Cookie": {u.cookie}}
	if origin != "" {
		header.Set("Origin", origin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, response, err := ws.Dial(ctx, f.wsURL(), &ws.DialOptions{HTTPHeader: header})
	if conn != nil {
		f.t.Cleanup(func() { _ = conn.CloseNow() })
	}
	return conn, response, err
}

// client reads its connection continuously, as a browser does. The library
// closes a connection whose Read context is cancelled, so tests must not
// poll with timeouts; they consume this channel instead.
type client struct {
	conn   *ws.Conn
	frames chan realtime.Frame
	done   chan error // receives the read error that ended the connection
}

func (f *fixture) mustDial(u user) *client {
	f.t.Helper()
	conn, _, err := f.dial(u, "")
	if err != nil {
		f.t.Fatalf("dial: %v", err)
	}
	return startClient(conn)
}

func startClient(conn *ws.Conn) *client {
	c := &client{conn: conn, frames: make(chan realtime.Frame, 256), done: make(chan error, 1)}
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				c.done <- err
				return
			}
			var frame realtime.Frame
			if err := json.Unmarshal(data, &frame); err != nil {
				c.done <- err
				return
			}
			c.frames <- frame
		}
	}()
	return c
}

func (c *client) send(t testing.TB, message any) {
	t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	c.sendRaw(t, ws.MessageText, data)
}

func (c *client) sendRaw(t testing.TB, kind ws.MessageType, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, kind, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (c *client) expect(t testing.TB, kind string) realtime.Frame {
	t.Helper()
	select {
	case frame := <-c.frames:
		if frame.Type != kind {
			t.Fatalf("got frame %+v, want type %q", frame, kind)
		}
		return frame
	case err := <-c.done:
		t.Fatalf("waiting for %q: %v", kind, err)
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %q", kind)
	}
	return realtime.Frame{}
}

func (c *client) expectSilence(t testing.TB, wait time.Duration) {
	t.Helper()
	select {
	case frame := <-c.frames:
		t.Fatalf("unexpected frame %+v", frame)
	case err := <-c.done:
		t.Fatalf("connection closed: %v", err)
	case <-time.After(wait):
	}
}

// expectClose waits for the server to close the connection and returns the
// close status, skipping frames that were already in flight.
func (c *client) expectClose(t testing.TB) ws.StatusCode {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case <-c.frames:
		case err := <-c.done:
			status := ws.CloseStatus(err)
			if status == -1 {
				t.Fatalf("expected a close frame, got %v", err)
			}
			return status
		case <-timeout:
			t.Fatal("connection was not closed")
		}
	}
}

func waitFor(t testing.TB, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSubscribeAndReceiveEvents(t *testing.T) {
	t.Parallel()
	f := newFixture(t, realtime.Options{}, websocket.Options{})
	conn := f.mustDial(f.user(f.tenant, "orders:read"))

	conn.send(t, map[string]string{"type": "subscribe", "topic": "orders", "ref": "r1"})
	if ack := conn.expect(t, "subscribed"); ack.Topic != "orders" {
		t.Fatalf("ack=%+v", ack)
	}
	sent := f.publish(f.tenant, "orders", "order.created", map[string]int{"id": 9})
	frame := conn.expect(t, "event")
	if frame.Topic != "orders" || frame.Event != "order.created" || frame.ID != sent.ID.String() || string(frame.Data) != `{"id":9}` {
		t.Fatalf("frame=%+v", frame)
	}

	conn.send(t, map[string]string{"type": "unsubscribe", "topic": "orders", "ref": "r2"})
	if ack := conn.expect(t, "unsubscribed"); ack.Ref != "r2" || ack.Topic != "orders" {
		t.Fatalf("ack=%+v", ack)
	}
	f.publish(f.tenant, "orders", "order.created", 2)
	conn.expectSilence(t, 150*time.Millisecond)

	conn.send(t, map[string]string{"type": "ping", "ref": "p1"})
	if pong := conn.expect(t, "pong"); pong.Ref != "p1" {
		t.Fatalf("pong=%+v", pong)
	}
}

func TestResumeWithLastEventID(t *testing.T) {
	t.Parallel()
	f := newFixture(t, realtime.Options{}, websocket.Options{})
	var ids []string
	for i := range 3 {
		ids = append(ids, f.publish(f.tenant, "orders", "order.created", i).ID.String())
	}
	conn := f.mustDial(f.user(f.tenant, "orders:read"))
	conn.send(t, map[string]string{"type": "subscribe", "topic": "orders", "last_event_id": ids[0]})
	conn.expect(t, "subscribed")
	for _, want := range ids[1:] {
		if got := conn.expect(t, "event"); got.ID != want {
			t.Fatalf("replayed %s, want %s", got.ID, want)
		}
	}
	conn.send(t, map[string]string{"type": "subscribe", "topic": "orders", "last_event_id": "nope"})
	if e := conn.expect(t, "error"); e.Code != "invalid_message" {
		t.Fatalf("error=%+v", e)
	}
}

func TestUnauthorizedSubscriptionIsRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t, realtime.Options{}, websocket.Options{})
	conn := f.mustDial(f.user(f.tenant, "orders:read"))

	for _, topic := range []string{"room:1", "undeclared", "orders:extra"} {
		conn.send(t, map[string]string{"type": "subscribe", "topic": topic, "ref": "x"})
		frame := conn.expect(t, "error")
		if frame.Code != "realtime_subscription_denied" || frame.Ref != "x" || frame.Topic != topic {
			t.Fatalf("topic %q: %+v", topic, frame)
		}
	}
	conn.send(t, map[string]string{"type": "subscribe", "topic": "Bad Topic\n<script>"})
	if frame := conn.expect(t, "error"); frame.Code != "realtime_topic_invalid" || frame.Topic != "" {
		t.Fatalf("invalid topic reflected or accepted: %+v", frame)
	}
	f.publish(f.tenant, "room:1", "message.created", "secret")
	conn.expectSilence(t, 150*time.Millisecond)
	if got := f.broker.Stats().Subscriptions; got != 0 {
		t.Fatalf("subscriptions=%d after only denied requests", got)
	}
}

func TestTenantIsolationOverWebSocket(t *testing.T) {
	t.Parallel()
	f := newFixture(t, realtime.Options{}, websocket.Options{})
	other := newTenant(t)
	a := f.mustDial(f.user(f.tenant, "orders:read"))
	b := f.mustDial(f.user(other, "orders:read"))
	for _, conn := range []*client{a, b} {
		conn.send(t, map[string]string{"type": "subscribe", "topic": "orders"})
		conn.expect(t, "subscribed")
	}
	f.publish(f.tenant, "orders", "order.created", "A only")
	if got := a.expect(t, "event"); string(got.Data) != `"A only"` {
		t.Fatalf("a got %+v", got)
	}
	b.expectSilence(t, 150*time.Millisecond)
}

func TestCrossOriginHandshakeIsRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t, realtime.Options{}, websocket.Options{Origins: realtime.OriginPolicy{Allowed: []string{"https://app.example.com"}}})
	u := f.user(f.tenant, "orders:read")
	sameOrigin := f.server.URL // http://127.0.0.1:port, equal to the Host header

	cases := []struct {
		name   string
		origin string
		allow  bool
	}{
		{"same origin", sameOrigin, true},
		{"allowlisted", "https://app.example.com", true},
		{"no origin header (non-browser client)", "", true},
		{"cross-site page with the victim's cookie", "https://evil.example", false},
		{"look-alike host", "https://app.example.com.evil.example", false},
		{"null origin (sandboxed frame)", "null", false},
		{"other port on the same host", "http://127.0.0.1:1", false},
	}
	for _, c := range cases {
		conn, response, err := f.dial(u, c.origin)
		switch {
		case c.allow && err != nil:
			t.Errorf("%s: rejected: %v", c.name, err)
		case !c.allow && err == nil:
			t.Errorf("%s: accepted", c.name)
		case !c.allow && (response == nil || response.StatusCode != http.StatusForbidden):
			t.Errorf("%s: response=%v, want 403 before any upgrade", c.name, response)
		}
		if conn != nil {
			_ = conn.CloseNow()
		}
	}
	waitFor(t, func() bool { return f.broker.Stats().Conns == 0 })

	strict := newFixture(t, realtime.Options{}, websocket.Options{Origins: realtime.OriginPolicy{RequireOrigin: true}})
	if _, _, err := strict.dial(strict.user(strict.tenant, "orders:read"), ""); err == nil {
		t.Error("RequireOrigin accepted a handshake without Origin")
	}
}

func TestHandshakeRequiresAuthentication(t *testing.T) {
	t.Parallel()
	f := newFixture(t, realtime.Options{}, websocket.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, response, err := ws.Dial(ctx, f.wsURL(), nil)
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous dial: response=%v err=%v", response, err)
	}
	_, response, err = ws.Dial(ctx, f.wsURL(), &ws.DialOptions{HTTPHeader: http.Header{"Cookie": {auth.SessionCookieName + "=forged"}}})
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged cookie dial: response=%v err=%v", response, err)
	}
}

func TestOversizedAndMalformedMessages(t *testing.T) {
	t.Parallel()
	f := newFixture(t, realtime.Options{}, websocket.Options{ReadLimit: 256})
	u := f.user(f.tenant, "orders:read")

	t.Run("oversized message closes the connection", func(t *testing.T) {
		conn := f.mustDial(u)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.conn.Write(ctx, ws.MessageText, []byte(`{"type":"ping","ref":"`+strings.Repeat("a", 1024)+`"}`))
		if status := conn.expectClose(t); status != ws.StatusMessageTooBig {
			t.Fatalf("close status=%d, want %d", status, ws.StatusMessageTooBig)
		}
	})

	t.Run("binary frames", func(t *testing.T) {
		conn := f.mustDial(u)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.conn.Write(ctx, ws.MessageBinary, []byte{1, 2, 3})
		if status := conn.expectClose(t); status != ws.StatusUnsupportedData {
			t.Fatalf("close status=%d", status)
		}
	})

	t.Run("invalid messages get errors and eventually a close", func(t *testing.T) {
		conn := f.mustDial(u)
		bad := []string{
			`not json`, `[]`, `{"type":"explode"}`, `{"type":"subscribe","topic":"orders","extra":1}`,
			`{"type":"ping","ref":"has spaces"}`, `{"type":"ping"} {"type":"ping"}`,
		}
		for _, message := range bad {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := conn.conn.Write(ctx, ws.MessageText, []byte(message)); err != nil {
				t.Fatal(err)
			}
			cancel()
			if frame := conn.expect(t, "error"); frame.Code != "invalid_message" && frame.Code != "invalid_ref" {
				t.Fatalf("%q: %+v", message, frame)
			}
		}
		// The connection survives a few mistakes...
		conn.send(t, map[string]string{"type": "ping"})
		conn.expect(t, "pong")
		// ...but not a stream of them.
		for range 10 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = conn.conn.Write(ctx, ws.MessageText, []byte(`nope`))
			cancel()
		}
		if status := conn.expectClose(t); status != ws.StatusPolicyViolation {
			t.Fatalf("close status=%d", status)
		}
	})
}

func TestMessageRateLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t, realtime.Options{}, websocket.Options{MessagesPerSecond: 5})
	conn := f.mustDial(f.user(f.tenant, "orders:read"))
	go func() {
		for range 100 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := conn.conn.Write(ctx, ws.MessageText, []byte(`{"type":"ping"}`))
			cancel()
			if err != nil {
				return
			}
		}
	}()
	if status := conn.expectClose(t); status != ws.StatusPolicyViolation {
		t.Fatalf("close status=%d", status)
	}
}

func TestKeepalive(t *testing.T) {
	t.Parallel()
	f := newFixture(t, realtime.Options{}, websocket.Options{PingInterval: 100 * time.Millisecond, PingTimeout: time.Second})
	u := f.user(f.tenant, "orders:read")

	// A client that reads answers pings automatically and stays connected
	// across many ping intervals.
	alive := f.mustDial(u)
	alive.send(t, map[string]string{"type": "subscribe", "topic": "orders"})
	alive.expect(t, "subscribed")
	alive.expectSilence(t, 500*time.Millisecond) // about 5 pings, all answered

	// A client that never reads cannot answer the ping and is dropped.
	deaf, _, err := f.dial(u, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = deaf // no Read: the library answers pings only while a Read is in progress
	waitFor(t, func() bool { return f.broker.Stats().Conns == 2 })
	waitFor(t, func() bool { return f.broker.Stats().Conns == 1 })
	f.publish(f.tenant, "orders", "order.created", 1)
	if frame := alive.expect(t, "event"); frame.Event != "order.created" {
		t.Fatalf("frame=%+v", frame)
	}
}

func TestShutdownSendsGoingAway(t *testing.T) {
	t.Parallel()
	f := newFixture(t, realtime.Options{}, websocket.Options{})
	conn := f.mustDial(f.user(f.tenant, "orders:read"))
	conn.send(t, map[string]string{"type": "subscribe", "topic": "orders"})
	conn.expect(t, "subscribed")

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.broker.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	if status := conn.expectClose(t); status != ws.StatusGoingAway {
		t.Fatalf("close status=%d", status)
	}
	waitFor(t, func() bool { return f.broker.Stats().Conns == 0 })
}

func TestClientThatStopsReadingIsDropped(t *testing.T) {
	t.Parallel()
	f := newFixture(t, realtime.Options{QueueSize: 8, MaxPayloadBytes: 64 << 10}, websocket.Options{WriteTimeout: 300 * time.Millisecond})
	conn := f.mustDial(f.user(f.tenant, "orders:read"))
	conn.send(t, map[string]string{"type": "subscribe", "topic": "orders"})
	conn.expect(t, "subscribed")
	// The client now stops reading entirely while the server keeps publishing
	// large events: socket buffers fill, writes stall, and the queue overflows
	// or the write times out. Either way the connection must be released and
	// publishing must stay fast.
	payload := strings.Repeat("x", 60<<10)
	start := time.Now()
	for i := 0; i < 2000 && f.broker.Stats().Conns == 1; i++ {
		f.publish(f.tenant, "orders", "order.created", payload)
		if i%8 == 7 {
			time.Sleep(time.Millisecond)
		}
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("publishing stalled for %s", elapsed)
	}
	waitFor(t, func() bool { return f.broker.Stats().Conns == 0 })
}

func TestConnectionChurnDoesNotLeakGoroutines(t *testing.T) {
	f := newFixture(t, realtime.Options{MaxConnsPerSubject: 200}, websocket.Options{})
	u := f.user(f.tenant, "orders:read")
	run := func(n int) {
		conns := make([]*client, n)
		for i := range conns {
			conns[i] = f.mustDial(u)
			conns[i].send(t, map[string]string{"type": "subscribe", "topic": "orders"})
			conns[i].expect(t, "subscribed")
		}
		f.publish(f.tenant, "orders", "order.created", 1)
		for i, conn := range conns {
			if i%2 == 0 {
				_ = conn.conn.Close(ws.StatusNormalClosure, "bye")
			} else {
				_ = conn.conn.CloseNow() // abrupt disconnect
			}
		}
		waitFor(t, func() bool { s := f.broker.Stats(); return s.Conns == 0 && s.Subscriptions == 0 })
	}
	run(3) // warm up lazily started goroutines
	http.DefaultClient.CloseIdleConnections()
	before := runtime.NumGoroutine()
	run(50)
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+2 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines %d, want <= %d\n%s", runtime.NumGoroutine(), before+2, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
