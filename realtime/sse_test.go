package realtime_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	forge "github.com/fabriciobonjorno/forge-go"
	"github.com/fabriciobonjorno/forge-go/auth"
	"github.com/fabriciobonjorno/forge-go/config"
	"github.com/fabriciobonjorno/forge-go/realtime"
	"github.com/fabriciobonjorno/forge-go/tenancy"
	"github.com/fabriciobonjorno/forge-go/uuid"
)

// sseBlock is one parsed text/event-stream block.
type sseBlock struct {
	id, event, data string
	retry           string
	comment         string
}

func (b sseBlock) frame(t testing.TB) realtime.Frame {
	t.Helper()
	var frame realtime.Frame
	if err := json.Unmarshal([]byte(b.data), &frame); err != nil {
		t.Fatalf("data %q is not a frame: %v", b.data, err)
	}
	return frame
}

type sseClient struct {
	response *http.Response
	blocks   chan sseBlock
	cancel   context.CancelFunc
}

// openStream connects to a stream and parses it in the background.
func openStream(t testing.TB, target string, header http.Header) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, values := range header {
		request.Header[key] = values
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &sseClient{response: response, blocks: make(chan sseBlock, 256), cancel: cancel}
	t.Cleanup(c.close)
	if response.StatusCode != http.StatusOK {
		return c
	}
	go func() {
		defer close(c.blocks)
		scanner := bufio.NewScanner(response.Body)
		var block sseBlock
		var data []string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				block.data = strings.Join(data, "\n")
				if block != (sseBlock{}) || len(data) > 0 {
					c.blocks <- block
				}
				block, data = sseBlock{}, nil
			case strings.HasPrefix(line, ":"):
				block.comment = strings.TrimSpace(line[1:])
			case strings.HasPrefix(line, "id: "):
				block.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				block.event = line[7:]
			case strings.HasPrefix(line, "retry: "):
				block.retry = line[7:]
			case strings.HasPrefix(line, "data: "):
				data = append(data, line[6:])
			default:
				c.blocks <- sseBlock{comment: "MALFORMED LINE: " + line}
			}
		}
	}()
	return c
}

func (c *sseClient) close() {
	c.cancel()
	_ = c.response.Body.Close()
}

// nextBlock returns the next block that matches, skipping others.
func (c *sseClient) nextBlock(t testing.TB, match func(sseBlock) bool) sseBlock {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case block, ok := <-c.blocks:
			if !ok {
				t.Fatal("stream ended while waiting for a block")
			}
			if strings.HasPrefix(block.comment, "MALFORMED") {
				t.Fatal(block.comment)
			}
			if match(block) {
				return block
			}
		case <-timeout:
			t.Fatal("timed out waiting for an SSE block")
		}
	}
}

func appEvent(b sseBlock) bool {
	return b.event != "" && !strings.HasPrefix(b.event, realtime.SyntheticPrefix)
}
func eventNamed(n string) func(sseBlock) bool { return func(b sseBlock) bool { return b.event == n } }
func heartbeat(b sseBlock) bool               { return b.comment == "heartbeat" }

func (c *sseClient) ended(t testing.TB) bool {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-c.blocks:
			if !ok {
				return true
			}
		case <-timeout:
			return false
		}
	}
}

type sseFixture struct {
	server *httptest.Server
	broker *realtime.Broker
	dir    *directory
	tenant tenancy.Tenant
}

func newSSEFixture(t *testing.T, options realtime.Options, sse realtime.SSEOptions) *sseFixture {
	t.Helper()
	d := newDirectory()
	if options.Channels == nil {
		options.Channels = []realtime.Channel{ordersRead, {Name: "room:{id}", Permission: "rooms:read"}}
	}
	broker := newBroker(t, options)
	handler, err := realtime.SSEHandler(broker, sse)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(d.middleware(t).Authenticate(handler))
	t.Cleanup(server.Close)
	return &sseFixture{server: server, broker: broker, dir: d, tenant: newTenant(t)}
}

func (f *sseFixture) url(topics ...string) string {
	query := url.Values{"topic": topics}
	return f.server.URL + "/events?" + query.Encode()
}

func bearer(u user) http.Header { return http.Header{"Authorization": {"Bearer " + u.token}} }

func TestSSEStreamsFramedEvents(t *testing.T) {
	t.Parallel()
	f := newSSEFixture(t, realtime.Options{}, realtime.SSEOptions{HeartbeatInterval: 30 * time.Millisecond, Retry: 1500 * time.Millisecond})
	reader := f.dir.add(t, f.tenant, "orders:read")
	stream := openStream(t, f.url("orders"), bearer(reader))

	if got := stream.response.Header; got.Get("Content-Type") != "text/event-stream; charset=utf-8" ||
		got.Get("Cache-Control") != "no-store, no-transform" || got.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers=%v", got)
	}
	first := stream.nextBlock(t, func(sseBlock) bool { return true })
	if first.retry != "1500" {
		t.Fatalf("first block=%+v, want retry: 1500", first)
	}
	ack := stream.nextBlock(t, eventNamed(realtime.EventSubscribed))
	if frame := ack.frame(t); frame.Type != "subscribed" || frame.Topic != "orders" || ack.id != "" {
		t.Fatalf("ack=%+v frame=%+v", ack, frame)
	}

	sent, err := f.broker.Publish(f.dir.context(t, f.dir.add(t, f.tenant)), "orders", "order.created", map[string]any{"id": 7, "note": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	block := stream.nextBlock(t, appEvent)
	frame := block.frame(t)
	if block.id != sent.ID.String() || block.event != "order.created" || frame.Type != "event" || frame.Topic != "orders" ||
		frame.Event != "order.created" || frame.ID != sent.ID.String() || string(frame.Data) != `{"id":7,"note":"ok"}` {
		t.Fatalf("block=%+v frame=%+v", block, frame)
	}
	if _, err := time.Parse(time.RFC3339Nano, frame.Time); err != nil {
		t.Fatalf("time %q: %v", frame.Time, err)
	}
	stream.nextBlock(t, heartbeat) // heartbeats keep flowing on an idle stream
}

func TestSSEPayloadsCannotInjectFields(t *testing.T) {
	t.Parallel()
	f := newSSEFixture(t, realtime.Options{}, realtime.SSEOptions{})
	stream := openStream(t, f.url("orders"), bearer(f.dir.add(t, f.tenant, "orders:read")))
	nasty := "line1\nevent: evil\r\nid: forged\rdata: more end"
	if _, err := f.broker.Publish(f.dir.context(t, f.dir.add(t, f.tenant)), "orders", "order.created", map[string]string{"note": nasty}); err != nil {
		t.Fatal(err)
	}
	block := stream.nextBlock(t, appEvent)
	if block.event != "order.created" || strings.Contains(block.id, "forged") {
		t.Fatalf("payload injected fields: %+v", block)
	}
	var payload map[string]string
	if err := json.Unmarshal(block.frame(t).Data, &payload); err != nil || payload["note"] != nasty {
		t.Fatalf("payload did not round-trip: %v %v", payload, err)
	}
	// The stream is still well formed afterwards.
	if _, err := f.broker.Publish(f.dir.context(t, f.dir.add(t, f.tenant)), "orders", "order.updated", 1); err != nil {
		t.Fatal(err)
	}
	stream.nextBlock(t, eventNamed("order.updated"))
}

func TestSSERejectsBeforeStreaming(t *testing.T) {
	t.Parallel()
	f := newSSEFixture(t, realtime.Options{}, realtime.SSEOptions{MaxTopics: 2})
	reader := f.dir.add(t, f.tenant, "orders:read")
	status := func(target string, header http.Header, mutate func(*http.Request)) (int, string) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, target, nil)
		request.Header = header.Clone()
		if mutate != nil {
			mutate(request)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body)
	}
	cases := []struct {
		name   string
		target string
		header http.Header
		mutate func(*http.Request)
		want   int
		code   string
	}{
		{"unauthenticated", f.url("orders"), nil, nil, http.StatusUnauthorized, "credentials_required"},
		{"no topic", f.url(), bearer(reader), nil, http.StatusBadRequest, "realtime_topics_required"},
		{"invalid topic", f.url("Orders"), bearer(reader), nil, http.StatusBadRequest, "realtime_topic_invalid"},
		{"too many topics", f.url("a", "b", "c"), bearer(reader), nil, http.StatusBadRequest, "realtime_too_many_topics"},
		{"undeclared topic", f.url("secrets"), bearer(reader), nil, http.StatusForbidden, "realtime_subscription_denied"},
		{"missing permission", f.url("room:1"), bearer(reader), nil, http.StatusForbidden, "realtime_subscription_denied"},
		{"one denied topic denies the stream", f.url("orders", "room:1"), bearer(reader), nil, http.StatusForbidden, "realtime_subscription_denied"},
		{"cross-site origin", f.url("orders"), bearer(reader), func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden, "realtime_origin_denied"},
		{"wrong method", f.url("orders"), bearer(reader), func(r *http.Request) { r.Method = http.MethodPost }, http.StatusMethodNotAllowed, "method_not_allowed"},
	}
	for _, c := range cases {
		code, body := status(c.target, c.header, c.mutate)
		if code != c.want || !strings.Contains(body, c.code) {
			t.Errorf("%s: status=%d body=%s, want %d %s", c.name, code, body, c.want, c.code)
		}
	}
	if conns := f.broker.Stats().Conns; conns != 0 {
		t.Fatalf("rejected requests left %d connections registered", conns)
	}
	// A same-origin browser request (Origin equals Host) is accepted.
	stream := openStream(t, f.url("orders"), bearer(reader))
	_ = stream
	same := openStream(t, f.url("orders"), http.Header{
		"Authorization": {"Bearer " + reader.token},
		"Origin":        {"http://" + strings.TrimPrefix(f.server.URL, "http://")},
	})
	if same.response.StatusCode != http.StatusOK {
		t.Fatalf("same-origin status=%d", same.response.StatusCode)
	}
}

func TestSSEResumesFromLastEventID(t *testing.T) {
	t.Parallel()
	f := newSSEFixture(t, realtime.Options{}, realtime.SSEOptions{})
	reader := f.dir.add(t, f.tenant, "orders:read")
	publisher := f.dir.context(t, f.dir.add(t, f.tenant))
	var ids []string
	for i := range 4 {
		event, err := f.broker.Publish(publisher, "orders", "order.created", i)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, event.ID.String())
	}

	header := bearer(reader)
	header.Set("Last-Event-ID", ids[1])
	stream := openStream(t, f.url("orders"), header)
	for _, want := range ids[2:] {
		if got := stream.nextBlock(t, appEvent); got.id != want {
			t.Fatalf("replayed %s, want %s", got.id, want)
		}
	}
	// Live events follow with no duplicate and no gap marker.
	live, _ := f.broker.Publish(publisher, "orders", "order.created", "live")
	if got := stream.nextBlock(t, appEvent); got.id != live.ID.String() {
		t.Fatalf("live event %s, want %s", got.id, live.ID)
	}

	// The query parameter works for clients that cannot set headers; a
	// malformed ID is ignored instead of failing the stream.
	viaQuery := openStream(t, f.url("orders")+"&last_event_id="+ids[2], bearer(reader))
	if got := viaQuery.nextBlock(t, appEvent); got.id != ids[3] {
		t.Fatalf("query resume replayed %s, want %s", got.id, ids[3])
	}
	bad := bearer(reader)
	bad.Set("Last-Event-ID", "not-a-uuid")
	fresh := openStream(t, f.url("orders"), bad)
	if fresh.response.StatusCode != http.StatusOK {
		t.Fatalf("malformed Last-Event-ID status=%d", fresh.response.StatusCode)
	}

	// An ID older than this process reports a gap instead of resuming silently.
	old, _ := uuid.NewGenerator(func() time.Time { return time.Now().Add(-time.Hour) }, zeros{})
	oldID, _ := old.New()
	gapped := bearer(reader)
	gapped.Set("Last-Event-ID", oldID.String())
	resync := openStream(t, f.url("orders"), gapped).nextBlock(t, eventNamed(realtime.EventResync))
	if frame := resync.frame(t); frame.Type != "resync" || frame.Topic != "orders" || resync.id != "" {
		t.Fatalf("resync=%+v frame=%+v", resync, frame)
	}
}

func TestSSEClientDisconnectCleansUp(t *testing.T) {
	f := newSSEFixture(t, realtime.Options{MaxConnsPerSubject: 100}, realtime.SSEOptions{HeartbeatInterval: 20 * time.Millisecond})
	reader := f.dir.add(t, f.tenant, "orders:read")
	// Warm the HTTP stack so lazily started goroutines are in the baseline.
	warm := openStream(t, f.url("orders"), bearer(reader))
	warm.nextBlock(t, eventNamed(realtime.EventSubscribed))
	warm.close()
	waitFor(t, func() bool { return f.broker.Stats().Conns == 0 })
	http.DefaultClient.CloseIdleConnections()
	before := runtime.NumGoroutine()

	const streams = 40
	clients := make([]*sseClient, streams)
	for i := range clients {
		clients[i] = openStream(t, f.url("orders"), bearer(reader))
	}
	for _, c := range clients {
		c.nextBlock(t, eventNamed(realtime.EventSubscribed))
	}
	if got := f.broker.Stats(); got.Conns != streams || got.Subscriptions != streams {
		t.Fatalf("stats=%+v", got)
	}
	for _, c := range clients {
		c.close()
	}
	waitFor(t, func() bool { s := f.broker.Stats(); return s.Conns == 0 && s.Subscriptions == 0 })
	settleGoroutines(t, before+2)
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

func TestSSEEndsWhenConsumerIsTooSlowOrBrokerCloses(t *testing.T) {
	t.Parallel()
	f := newSSEFixture(t, realtime.Options{QueueSize: 4}, realtime.SSEOptions{})
	reader := f.dir.add(t, f.tenant, "orders:read")
	stream := openStream(t, f.url("orders"), bearer(reader))
	stream.nextBlock(t, eventNamed(realtime.EventSubscribed))

	// Close the broker: the stream ends instead of holding the server open.
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.broker.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	if !stream.ended(t) {
		t.Fatal("stream stayed open after the broker closed")
	}
	// New streams are refused with 503 while shutting down.
	request, _ := http.NewRequest(http.MethodGet, f.url("orders"), nil)
	request.Header = bearer(reader)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status after close=%d", response.StatusCode)
	}
}

// runApp starts a real Forge application (net/http server with its production
// timeouts) on a free port and returns its base URL.
func runApp(t *testing.T, parent context.Context, cfg config.Config, configure func(*forge.App)) (base string, stop func() error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTP.Address = listener.Addr().String()
	_ = listener.Close()
	cfg.Environment = config.Test
	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	configure(app)
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	base = "http://" + cfg.HTTP.Address
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get(base + "/health/live")
		if err == nil {
			_ = response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server never started: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var stopped atomic.Bool
	stop = func() error {
		cancel()
		select {
		case err := <-done:
			stopped.Store(true)
			return err
		case <-time.After(10 * time.Second):
			return errors.New("application did not stop")
		}
	}
	t.Cleanup(func() {
		if !stopped.Load() {
			_ = stop()
		}
	})
	return base, stop
}

func TestSSEOutlivesServerTimeoutsOnTheRealServer(t *testing.T) {
	d := newDirectory()
	tenant := newTenant(t)
	cfg := config.Default()
	cfg.HTTP.ReadTimeout = 250 * time.Millisecond
	cfg.HTTP.WriteTimeout = 250 * time.Millisecond
	cfg.HTTP.IdleTimeout = time.Second
	cfg.HTTP.ShutdownTimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	broker, err := realtime.New(ctx, realtime.Options{Logger: discard, Channels: []realtime.Channel{ordersRead}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := realtime.SSEHandler(broker, realtime.SSEOptions{HeartbeatInterval: 60 * time.Millisecond, WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	base, stop := runApp(t, ctx, cfg, func(a *forge.App) {
		// Browser streams authenticate with the session cookie.
		if err := a.Handle("GET /events", d.middleware(t).AuthenticateCookie(handler)); err != nil {
			t.Fatal(err)
		}
		// Control: an ordinary handler is still bound by WriteTimeout.
		if err := a.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(600 * time.Millisecond)
			_, _ = io.WriteString(w, strings.Repeat("late", 1<<16))
		}); err != nil {
			t.Fatal(err)
		}
		if err := a.OnShutdown(broker.Close); err != nil {
			t.Fatal(err)
		}
	})

	reader := d.add(t, tenant, "orders:read")
	cookie := &http.Cookie{Name: auth.SessionCookieName, Value: reader.token}
	header := http.Header{"Cookie": {cookie.String()}}

	// Control: the server really does cut ordinary responses at WriteTimeout.
	if response, err := http.Get(base + "/slow"); err == nil {
		_, readErr := io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if readErr == nil {
			t.Fatal("control request outlived WriteTimeout; the test would prove nothing")
		}
	}

	stream := openStream(t, base+"/events?topic=orders", header)
	if stream.response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", stream.response.StatusCode)
	}
	stream.nextBlock(t, eventNamed(realtime.EventSubscribed))

	publisher := d.context(t, d.add(t, tenant))
	start := time.Now()
	heartbeats, delivered := 0, 0
	nextPublish := 0
	for time.Since(start) < 1500*time.Millisecond { // six times WriteTimeout and ReadTimeout
		select {
		case block, ok := <-stream.blocks:
			if !ok {
				t.Fatalf("stream ended after %s (WriteTimeout=%s)", time.Since(start), cfg.HTTP.WriteTimeout)
			}
			switch {
			case heartbeat(block):
				heartbeats++
			case appEvent(block):
				delivered++
			}
		case <-time.After(20 * time.Millisecond):
		}
		if time.Since(start) > time.Duration(nextPublish)*400*time.Millisecond {
			nextPublish++
			if _, err := broker.Publish(publisher, "orders", "order.created", nextPublish); err != nil {
				t.Fatal(err)
			}
		}
	}
	if heartbeats < 10 {
		t.Fatalf("only %d heartbeats in %s", heartbeats, time.Since(start))
	}
	// Give the last published event time to arrive.
	deadline := time.Now().Add(2 * time.Second)
	for delivered < nextPublish && time.Now().Before(deadline) {
		if block := stream.nextBlock(t, func(b sseBlock) bool { return appEvent(b) }); block.id != "" {
			delivered++
		}
	}
	if delivered != nextPublish {
		t.Fatalf("delivered %d of %d events published across the timeouts", delivered, nextPublish)
	}

	// Shutting down the app while a stream is open must not wait for the
	// drain timeout: the broker follows the application's context.
	shutdownStarted := time.Now()
	// One signal context stops the HTTP server and the broker together.
	cancel()
	if err := stop(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if elapsed := time.Since(shutdownStarted); elapsed > 3*time.Second {
		t.Fatalf("shutdown with an open stream took %s", elapsed)
	}
	if !stream.ended(t) {
		t.Fatal("stream survived shutdown")
	}
}
