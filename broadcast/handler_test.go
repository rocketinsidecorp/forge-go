package broadcast_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/broadcast"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

type fixture struct {
	t      *testing.T
	server *httptest.Server
	memory *broadcast.Memory
	signer *broadcast.Signer
	clock  *clock
}

// newFixture serves the handler from a real http.Server whose read and write
// timeouts are far shorter than the streams under test. The test middleware
// plays the part of authentication: the X-Test-Tenant header becomes the
// request's tenant.
func newFixture(t *testing.T, opts broadcast.HandlerOptions, memoryOpts broadcast.MemoryOptions) *fixture {
	t.Helper()
	memory := newMemory(t, memoryOpts)
	signer, clk := newSigner(t, nil)
	opts.Broker, opts.Signer = memory, signer
	handler, err := broadcast.NewHandler(opts)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw := r.Header.Get("X-Test-Tenant"); raw != "" {
			id, err := uuid.Parse(raw)
			if err != nil {
				t.Error(err)
			}
			ctx, err := tenancy.WithContext(r.Context(), tenancy.Tenant{ID: id})
			if err != nil {
				t.Error(err)
			}
			r = r.WithContext(ctx)
		}
		handler.ServeHTTP(w, r)
	}))
	server.Config.ReadTimeout = 150 * time.Millisecond
	server.Config.WriteTimeout = 150 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)
	return &fixture{t: t, server: server, memory: memory, signer: signer, clock: clk}
}

func (f *fixture) token(channel broadcast.Channel) string {
	f.t.Helper()
	token, err := f.signer.Sign(channel)
	if err != nil {
		f.t.Fatal(err)
	}
	return token
}

func (f *fixture) get(ctx context.Context, token, tenant string) *http.Response {
	f.t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/streams?stream="+url.QueryEscape(token), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	request.Header.Set("Accept", "text/event-stream")
	if tenant != "" {
		request.Header.Set("X-Test-Tenant", tenant)
	}
	response, err := f.server.Client().Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// lines reads the response body line by line until it closes.
func lines(body io.Reader) <-chan string {
	out := make(chan string, 64)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(body)
		for scanner.Scan() {
			out <- scanner.Text()
		}
	}()
	return out
}

func next(t *testing.T, from <-chan string) (string, bool) {
	t.Helper()
	select {
	case line, ok := <-from:
		return line, ok
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading the stream")
		return "", false
	}
}

// expect reads lines until want appears and fails if the stream ends first.
func expect(t *testing.T, from <-chan string, want string) {
	t.Helper()
	for {
		line, ok := next(t, from)
		if !ok {
			t.Fatalf("stream ended before %q", want)
		}
		if line == want {
			return
		}
	}
}

func waitSubscribers(t *testing.T, memory *broadcast.Memory, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for memory.Subscribers() != want {
		if time.Now().After(deadline) {
			t.Fatalf("subscribers = %d, want %d", memory.Subscribers(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHandlerStreamsMessagesAndOutlivesServerTimeouts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{Heartbeat: 40 * time.Millisecond}, broadcast.MemoryOptions{})
	channel := mustGlobal(t, "posts")
	response := f.get(context.Background(), f.token(channel), "")
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" ||
		response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("status=%d headers=%v", response.StatusCode, response.Header)
	}
	stream := lines(response.Body)
	expect(t, stream, ": connected")
	expect(t, stream, ": ping") // heartbeat

	// The server's ReadTimeout and WriteTimeout (150ms) have passed several
	// times over by now; the stream must still be alive and deliver.
	time.Sleep(500 * time.Millisecond)
	if err := f.memory.Publish(context.Background(), channel, broadcast.Message{Data: "<turbo-stream action=\"remove\" target=\"post_1\"></turbo-stream>\n"}); err != nil {
		t.Fatal(err)
	}
	expect(t, stream, `data: <turbo-stream action="remove" target="post_1"></turbo-stream>`)
}

func TestHandlerEventFraming(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{}, broadcast.MemoryOptions{})
	channel := mustGlobal(t, "posts")
	response := f.get(context.Background(), f.token(channel), "")
	stream := lines(response.Body)
	expect(t, stream, ": connected")
	next(t, stream) // the blank line ending the comment event
	waitSubscribers(t, f.memory, 1)

	// Line terminators in the payload must not end the event or inject fields.
	payload := "one\ntwo\r\nthree\rid: injected\n\nevent: evil"
	if err := f.memory.Publish(context.Background(), channel, broadcast.Message{Data: payload}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		line, ok := next(t, stream)
		if !ok {
			t.Fatal("stream ended")
		}
		if line == "" {
			break
		}
		got = append(got, line)
	}
	want := []string{"data: one", "data: two", "data: three", "data: id: injected", "data: ", "data: event: evil"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("event lines = %q, want %q", got, want)
	}
}

func TestHandlerRefusesBadTokens(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{}, broadcast.MemoryOptions{})
	channel := mustGlobal(t, "posts")
	good := f.token(channel)
	tampered := []byte(good)
	tampered[len(tampered)-2] ^= 0x01
	expired := f.token(channel)
	f.clock.Advance(broadcast.DefaultTTL + time.Hour)

	for name, token := range map[string]string{
		"expired":  expired,
		"tampered": string(tampered),
		"garbage":  "nonsense",
		"empty":    "",
	} {
		response := f.get(context.Background(), token, "")
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "stream_invalid") {
			t.Errorf("%s: status=%d body=%s", name, response.StatusCode, body)
		}
	}
	if f.memory.Subscribers() != 0 {
		t.Fatal("a refused request subscribed")
	}

	// A missing or repeated parameter is refused too.
	for _, query := range []string{"", "?stream=a&stream=b", "?other=1"} {
		response, err := f.server.Client().Get(f.server.URL + "/streams" + query)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("query %q: status=%d", query, response.StatusCode)
		}
	}

	post, err := f.server.Client().Post(f.server.URL+"/streams?stream="+good, "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = post.Body.Close()
	if post.StatusCode != http.StatusMethodNotAllowed || post.Header.Get("Allow") != "GET" {
		t.Fatalf("POST status=%d", post.StatusCode)
	}
}

func TestHandlerTenantChannelsRequireTheSameTenant(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{}, broadcast.MemoryOptions{})
	mine, theirs := uuid.MustNew(), uuid.MustNew()
	token := f.token(mustTenant(t, mine, "posts"))

	for name, tenant := range map[string]string{"no tenant": "", "another tenant": theirs.String()} {
		response := f.get(context.Background(), token, tenant)
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "stream_forbidden") {
			t.Errorf("%s: status=%d body=%s", name, response.StatusCode, body)
		}
	}
	if f.memory.Subscribers() != 0 {
		t.Fatal("a refused request subscribed")
	}

	response := f.get(context.Background(), token, mine.String())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("own tenant: status=%d", response.StatusCode)
	}
	stream := lines(response.Body)
	expect(t, stream, ": connected")
	next(t, stream) // the blank line ending the comment event
	waitSubscribers(t, f.memory, 1)

	// Another tenant's channel of the same name, and the global one, are not
	// delivered here.
	other := mustTenant(t, theirs, "posts")
	_ = f.memory.Publish(context.Background(), other, broadcast.Message{Data: "other tenant"})
	_ = f.memory.Publish(context.Background(), mustGlobal(t, "posts"), broadcast.Message{Data: "global"})
	_ = f.memory.Publish(context.Background(), mustTenant(t, mine, "posts"), broadcast.Message{Data: "mine"})
	if line, _ := next(t, stream); line != "data: mine" {
		t.Fatalf("first message = %q, want the own-tenant message", line)
	}
}

func TestHandlerAuthorizeHook(t *testing.T) {
	t.Parallel()
	var seen []broadcast.Channel
	f := newFixture(t, broadcast.HandlerOptions{Authorize: func(r *http.Request, channel broadcast.Channel) error {
		seen = append(seen, channel)
		if r.Header.Get("X-Allow") != "yes" {
			return fault.New("permission_denied", "permission denied", fault.CategoryForbidden, 0)
		}
		return nil
	}}, broadcast.MemoryOptions{})
	channel := mustGlobal(t, "announcements")

	denied := f.get(context.Background(), f.token(channel), "")
	body, _ := io.ReadAll(denied.Body)
	if denied.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "permission_denied") || f.memory.Subscribers() != 0 {
		t.Fatalf("status=%d body=%s subscribers=%d", denied.StatusCode, body, f.memory.Subscribers())
	}

	request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/streams?stream="+f.token(channel), nil)
	request.Header.Set("X-Allow", "yes")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	allowed, err := f.server.Client().Do(request.WithContext(ctx))
	if err != nil || allowed.StatusCode != http.StatusOK {
		t.Fatalf("allowed request: %v", err)
	}
	defer func() { _ = allowed.Body.Close() }()
	if len(seen) != 2 || seen[0] != channel {
		t.Fatalf("hook saw %v", seen)
	}
}

func TestHandlerReleasesTheSubscriptionWhenTheClientLeaves(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{}, broadcast.MemoryOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	response := f.get(ctx, f.token(mustGlobal(t, "posts")), "")
	expect(t, lines(response.Body), ": connected")
	waitSubscribers(t, f.memory, 1)
	cancel()
	waitSubscribers(t, f.memory, 0)
}

func TestHandlerEndsWhenTheBrokerShutsDown(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{}, broadcast.MemoryOptions{})
	response := f.get(context.Background(), f.token(mustGlobal(t, "posts")), "")
	stream := lines(response.Body)
	expect(t, stream, ": connected")
	waitSubscribers(t, f.memory, 1)

	// Closing the broker (shutdown) ends open streams, which lets the HTTP
	// server drain instead of waiting out its grace period.
	f.memory.Close()
	for {
		if _, ok := next(t, stream); !ok {
			break
		}
	}
	refused, err := f.server.Client().Get(f.server.URL + "/streams?stream=" + f.token(mustGlobal(t, "posts")))
	if err != nil {
		t.Fatal(err)
	}
	_ = refused.Body.Close()
	if refused.StatusCode != http.StatusServiceUnavailable || refused.Header.Get("Retry-After") == "" {
		t.Fatalf("after shutdown: status=%d", refused.StatusCode)
	}
}

func TestHandlerSheddsLoadAtCapacity(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{}, broadcast.MemoryOptions{MaxSubscribers: 1})
	token := f.token(mustGlobal(t, "posts"))
	first := f.get(context.Background(), token, "")
	expect(t, lines(first.Body), ": connected")
	second := f.get(context.Background(), token, "")
	if second.StatusCode != http.StatusServiceUnavailable || second.Header.Get("Retry-After") != "5" {
		t.Fatalf("status=%d headers=%v", second.StatusCode, second.Header)
	}
}

func TestHandlerEndsStreamsAtMaxConnectionAge(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{MaxConnectionAge: 100 * time.Millisecond}, broadcast.MemoryOptions{})
	response := f.get(context.Background(), f.token(mustGlobal(t, "posts")), "")
	stream := lines(response.Body)
	for {
		if _, ok := next(t, stream); !ok {
			break
		}
	}
	waitSubscribers(t, f.memory, 0)
}

func TestHandlerOptionsAreValidated(t *testing.T) {
	t.Parallel()
	memory := newMemory(t, broadcast.MemoryOptions{})
	signer, _ := newSigner(t, nil)
	for name, opts := range map[string]broadcast.HandlerOptions{
		"no broker":          {Signer: signer},
		"no signer":          {Broker: memory},
		"negative heartbeat": {Broker: memory, Signer: signer, Heartbeat: -1},
		"negative timeout":   {Broker: memory, Signer: signer, WriteTimeout: -1},
	} {
		if _, err := broadcast.NewHandler(opts); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Over HTTP/2 a write deadline that passes resets the stream even when
// nothing is being written. The heartbeat here is longer than the handler's
// write timeout, so a deadline left set after a write would kill the idle
// stream before its first ping.
func TestHandlerStreamsSurviveTheWriteTimeoutOverHTTP2(t *testing.T) {
	t.Parallel()
	memory := newMemory(t, broadcast.MemoryOptions{})
	signer, _ := newSigner(t, nil)
	handler, err := broadcast.NewHandler(broadcast.HandlerOptions{
		Broker: memory, Signer: signer,
		Heartbeat: 900 * time.Millisecond, WriteTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	channel := mustGlobal(t, "posts")
	token, _ := signer.Sign(channel)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/streams?stream="+url.QueryEscape(token), nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusOK {
		t.Fatalf("proto=%s status=%d: the test needs HTTP/2", response.Proto, response.StatusCode)
	}
	stream := lines(response.Body)
	expect(t, stream, ": connected")
	expect(t, stream, ": ping") // after the write timeout has long passed
	waitSubscribers(t, memory, 1)
	if err := memory.Publish(ctx, channel, broadcast.Message{Data: "still here"}); err != nil {
		t.Fatal(err)
	}
	expect(t, stream, "data: still here")
}

func TestHandlerCapsOpenStreamsPerClientAndPerTenant(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{MaxPerPrincipal: 2, MaxPerTenant: 3}, broadcast.MemoryOptions{})
	global := f.token(mustGlobal(t, "announcements"))
	open := func(token, tenant string) *http.Response { return f.get(context.Background(), token, tenant) }

	// Without a principal, streams are counted per client address: one client
	// cannot take a whole (even global) channel's capacity.
	ctx1, cancel1 := context.WithCancel(context.Background())
	first := f.get(ctx1, global, "")
	f.get(context.Background(), global, "")
	limited := open(global, "")
	if limited.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third stream: status=%d, want 429", limited.StatusCode)
	}
	body, _ := io.ReadAll(limited.Body)
	if !strings.Contains(string(body), "stream_limit") || limited.Header.Get("Retry-After") == "" {
		t.Fatalf("third stream: status=%d body=%s", limited.StatusCode, body)
	}
	if f.memory.Subscribers() != 2 {
		t.Fatalf("subscribers = %d", f.memory.Subscribers())
	}
	// A closed stream frees its slot.
	_ = first.Body.Close()
	cancel1()
	waitSubscribers(t, f.memory, 1)
	if again := open(global, ""); again.StatusCode != http.StatusOK {
		t.Fatalf("after a stream closed: status=%d", again.StatusCode)
	}
}

func TestHandlerCapsOpenStreamsPerTenant(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{MaxPerPrincipal: -1, MaxPerTenant: 2}, broadcast.MemoryOptions{})
	mine, other := uuid.MustNew(), uuid.MustNew()
	mineToken := f.token(mustTenant(t, mine, "posts"))
	otherToken := f.token(mustTenant(t, other, "posts"))
	for range 2 {
		if response := f.get(context.Background(), mineToken, mine.String()); response.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", response.StatusCode)
		}
	}
	if response := f.get(context.Background(), mineToken, mine.String()); response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a tenant's third stream: status=%d", response.StatusCode)
	}
	// Another tenant is not affected by it.
	if response := f.get(context.Background(), otherToken, other.String()); response.StatusCode != http.StatusOK {
		t.Fatalf("another tenant: status=%d", response.StatusCode)
	}
}

func TestHandlerLimitsCanBeDisabled(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{MaxPerPrincipal: -1, MaxPerTenant: -1}, broadcast.MemoryOptions{})
	token := f.token(mustGlobal(t, "posts"))
	for range 20 {
		if response := f.get(context.Background(), token, ""); response.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", response.StatusCode)
		}
	}
}

func TestHandlerDefaultsLimitOneClientToSixteenStreams(t *testing.T) {
	t.Parallel()
	f := newFixture(t, broadcast.HandlerOptions{}, broadcast.MemoryOptions{})
	token := f.token(mustGlobal(t, "posts"))
	for i := range broadcast.DefaultMaxPerPrincipal {
		if response := f.get(context.Background(), token, ""); response.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: status=%d", i+1, response.StatusCode)
		}
	}
	if response := f.get(context.Background(), token, ""); response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("stream %d: status=%d, want 429", broadcast.DefaultMaxPerPrincipal+1, response.StatusCode)
	}
}
