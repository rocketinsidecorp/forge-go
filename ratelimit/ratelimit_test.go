package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/web"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)} }

func TestMemoryIsBoundedAndEvictsTheSoonestExpiring(t *testing.T) {
	c := newClock()
	m, err := newMemory(MemoryOptions{MaxKeys: 3}, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	limit := Limit{N: 1, Per: time.Minute}
	for i := range 3 {
		if _, err := m.Allow(ctx, fmt.Sprintf("k%d", i), limit); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Second)
	}
	// k0 is closest to expiring: a fourth key evicts it, never grows the map.
	if d, _ := m.Allow(ctx, "k3", limit); !d.Allowed {
		t.Fatal("new key must be allowed when evicting")
	}
	if got := m.Len(); got != 3 {
		t.Fatalf("Len = %d, want the bound 3", got)
	}
	if d, _ := m.Allow(ctx, "k0", limit); !d.Allowed {
		t.Fatal("k0 was evicted, so its counter restarts")
	}
	for i := range 10_000 {
		if _, err := m.Allow(ctx, fmt.Sprintf("flood-%d", i), limit); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.Len(); got != 3 {
		t.Fatalf("after a flood Len = %d, want 3", got)
	}
}

func TestMemoryDenyWhenFullKeepsEveryCounter(t *testing.T) {
	c := newClock()
	m, err := newMemory(MemoryOptions{MaxKeys: 2, WhenFull: Deny}, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	limit := Limit{N: 1, Per: time.Minute}
	_, _ = m.Allow(ctx, "a", limit)
	_, _ = m.Allow(ctx, "b", limit)
	d, _ := m.Allow(ctx, "c", limit)
	if d.Allowed || d.RetryAfter <= 0 {
		t.Fatalf("new key while full = %+v, want denied with a retry-after", d)
	}
	if d, _ := m.Allow(ctx, "a", limit); d.Allowed {
		t.Fatal("a's counter must survive")
	}
	c.Advance(time.Minute)
	if d, _ := m.Allow(ctx, "c", limit); !d.Allowed {
		t.Fatal("after the buckets expire the new key must be allowed")
	}
}

func TestMemoryExpiredBucketsLeaveTheMap(t *testing.T) {
	c := newClock()
	m, _ := newMemory(MemoryOptions{MaxKeys: 100}, c.Now)
	for i := range 50 {
		_, _ = m.Allow(context.Background(), fmt.Sprintf("k%d", i), Limit{N: 1, Per: time.Second})
	}
	c.Advance(2 * time.Second)
	_, _ = m.Allow(context.Background(), "fresh", Limit{N: 1, Per: time.Second})
	if got := m.Len(); got != 1 {
		t.Fatalf("Len = %d after expiry, want only the fresh bucket", got)
	}
}

func TestMemoryFixedWindowSemantics(t *testing.T) {
	c := newClock()
	m, _ := newMemory(MemoryOptions{}, c.Now)
	ctx := context.Background()
	limit := Limit{N: 2, Per: 10 * time.Second}
	_, _ = m.Allow(ctx, "k", limit)
	c.Advance(4 * time.Second)
	_, _ = m.Allow(ctx, "k", limit)
	d, _ := m.Allow(ctx, "k", limit)
	if d.Allowed || d.RetryAfter != 6*time.Second {
		t.Fatalf("denied = %+v, want RetryAfter 6s (window started at the first hit)", d)
	}
	c.Advance(6 * time.Second)
	if d, _ := m.Allow(ctx, "k", limit); !d.Allowed {
		t.Fatal("window must reset when it ends")
	}
}

func TestMemoryRaceAndNoGoroutines(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{MaxKeys: 64})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				_, _ = m.Allow(context.Background(), fmt.Sprintf("k%d", (g*7+i)%200), Limit{N: 3, Per: 5 * time.Millisecond})
			}
		}()
	}
	wg.Wait()
	if m.Len() > 64 {
		t.Fatalf("Len = %d, exceeds MaxKeys", m.Len())
	}
}

func TestNewMemoryValidation(t *testing.T) {
	for name, opts := range map[string]MemoryOptions{
		"negative keys": {MaxKeys: -1},
		"too many keys": {MaxKeys: maxMemoryKeys + 1},
		"bad policy":    {WhenFull: 7},
	} {
		if _, err := NewMemory(opts); err == nil {
			t.Errorf("%s: NewMemory succeeded", name)
		}
	}
}

func TestKeyIsDeterministicUnambiguousAndOpaque(t *testing.T) {
	if Key("s", "a", "bc") == Key("s", "ab", "c") {
		t.Fatal("length prefixes must keep part boundaries")
	}
	if Key("s", "a") == Key("t", "a") {
		t.Fatal("scope must matter")
	}
	k := Key("POST /login", "203.0.113.7")
	if k != Key("POST /login", "203.0.113.7") || len(k) != 64 {
		t.Fatalf("Key = %q, want a stable 64-character digest", k)
	}
	if err := CheckRequest(k, Limit{N: 1, Per: time.Second}); err != nil {
		t.Fatal(err)
	}
}

func TestByRemoteAddr(t *testing.T) {
	for remote, want := range map[string]string{
		"203.0.113.7:5555":                        "203.0.113.7",
		"[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:4444": "2001:db8:1:2::/64",
		"[2001:db8:1:2:1111:2222:3333:4444]:80":   "2001:db8:1:2::/64",
		"[::ffff:198.51.100.9]:1":                 "198.51.100.9",
		"[fe80::1%eth0]:9":                        "fe80::/64",
		"not-an-address":                          "not-an-address",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-For", "1.2.3.4")
		if got := ByRemoteAddr(r); got != want {
			t.Errorf("ByRemoteAddr(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestLimitValidate(t *testing.T) {
	good := []Limit{{1, time.Millisecond}, {MaxN, MaxPer}, {10, 3 * time.Minute}}
	for _, l := range good {
		if err := l.Validate(); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
	bad := []Limit{{0, time.Second}, {-1, time.Second}, {MaxN + 1, time.Second}, {1, 0}, {1, time.Microsecond}, {1, MaxPer + 1}}
	for _, l := range bad {
		if l.Validate() == nil {
			t.Errorf("%+v: want error", l)
		}
	}
}

func TestStoreOptionsNormalize(t *testing.T) {
	o, err := StoreOptions{}.Normalize()
	if err != nil || o.PruneInterval != time.Minute || o.PruneBatch != 500 {
		t.Fatalf("defaults = %+v, %v", o, err)
	}
	if _, err := (StoreOptions{PruneBatch: -1}).Normalize(); err == nil {
		t.Fatal("negative batch accepted")
	}
	if o, _ := (StoreOptions{PruneInterval: -1}).Normalize(); o.PruneInterval != -1 {
		t.Fatal("negative interval must stay (disables pruning)")
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	for d, want := range map[time.Duration]int{time.Millisecond: 1, time.Second: 1, 1001 * time.Millisecond: 2, 3 * time.Minute: 180} {
		if got := RetryAfterSeconds(d); got != want {
			t.Errorf("RetryAfterSeconds(%s) = %d, want %d", d, got, want)
		}
	}
}

// --- middleware ---

func okHandler(count *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
}

func do(h http.Handler, method, target, remote string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = remote
	for k, v := range header {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func newTestMiddleware(t *testing.T, limiter Limiter, key KeyFunc, limit Limit, opts ...Option) func(http.Handler) http.Handler {
	t.Helper()
	mw, err := Middleware(limiter, key, limit, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return mw
}

func TestMiddlewareLimitsPerRouteLikeRailsRateLimit(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	var created, listed atomic.Int32
	mw := newTestMiddleware(t, m, ByRemoteAddr, Limit{N: 3, Per: 3 * time.Minute})
	mux := http.NewServeMux()
	mux.Handle("POST /comments", mw(okHandler(&created)))
	mux.Handle("GET /comments", okHandler(&listed))

	for i := range 3 {
		if w := do(mux, "POST", "/comments", "203.0.113.7:1", nil); w.Code != http.StatusNoContent {
			t.Fatalf("request %d = %d", i+1, w.Code)
		}
	}
	w := do(mux, "POST", "/comments", "203.0.113.7:2", nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "180" {
		t.Fatalf("Retry-After = %q, want 180", got)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("429 must not be cached")
	}
	var body struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != "rate_limited" {
		t.Fatalf("body = %s (%v), want the JSON error format with code rate_limited", w.Body, err)
	}
	if created.Load() != 3 {
		t.Fatalf("handler ran %d times, want 3", created.Load())
	}
	if do(mux, "GET", "/comments", "203.0.113.7:3", nil).Code != http.StatusNoContent {
		t.Fatal("other routes are not limited")
	}
	if do(mux, "POST", "/comments", "198.51.100.1:3", nil).Code != http.StatusNoContent {
		t.Fatal("another client has its own bucket")
	}
}

func TestMiddlewareScopesRoutesSeparatelyByPattern(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	var n atomic.Int32
	mw := newTestMiddleware(t, m, ByRemoteAddr, Limit{N: 1, Per: time.Minute})
	mux := http.NewServeMux()
	mux.Handle("POST /a", mw(okHandler(&n)))
	mux.Handle("POST /b", mw(okHandler(&n)))
	if do(mux, "POST", "/a", "203.0.113.7:1", nil).Code != 204 || do(mux, "POST", "/b", "203.0.113.7:1", nil).Code != 204 {
		t.Fatal("distinct routes must be counted separately")
	}
	if do(mux, "POST", "/a", "203.0.113.7:1", nil).Code != 429 {
		t.Fatal("second hit on /a must be limited")
	}
}

func TestMiddlewareWithScopeSharesCounterAcrossRoutes(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	var n atomic.Int32
	mw := newTestMiddleware(t, m, ByRemoteAddr, Limit{N: 1, Per: time.Minute}, WithScope("writes"))
	mux := http.NewServeMux()
	mux.Handle("POST /a", mw(okHandler(&n)))
	mux.Handle("POST /b", mw(okHandler(&n)))
	if do(mux, "POST", "/a", "203.0.113.7:1", nil).Code != 204 {
		t.Fatal("first request")
	}
	if do(mux, "POST", "/b", "203.0.113.7:1", nil).Code != 429 {
		t.Fatal("same scope must share the counter")
	}
}

func TestMiddlewareDoesNotTrustForwardedHeaders(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	var n atomic.Int32
	h := newTestMiddleware(t, m, ByRemoteAddr, Limit{N: 1, Per: time.Minute})(okHandler(&n))
	spoof := func(ip string) http.Header {
		return http.Header{"X-Forwarded-For": {ip}, "X-Real-Ip": {ip}, "Forwarded": {"for=" + ip}}
	}
	if do(h, "GET", "/", "203.0.113.7:1", spoof("1.1.1.1")).Code != 204 {
		t.Fatal("first request")
	}
	if do(h, "GET", "/", "203.0.113.7:2", spoof("2.2.2.2")).Code != 429 {
		t.Fatal("rotating X-Forwarded-For must not evade the limit")
	}
}

func TestMiddlewareHTMLPageForBrowsers(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	var n atomic.Int32
	h := newTestMiddleware(t, m, ByRemoteAddr, Limit{N: 1, Per: time.Minute})(okHandler(&n))
	html := http.Header{"Accept": {"text/html,application/xhtml+xml;q=0.9,*/*;q=0.8"}}
	do(h, "GET", "/", "203.0.113.7:1", html)
	w := do(h, "GET", "/", "203.0.113.7:1", html)
	if w.Code != 429 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), "Too many requests") {
		t.Fatalf("response = %d %q %q, want a 429 HTML page", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}

func TestMiddlewareEmptyKeyIsLimitedNotExempt(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	var n atomic.Int32
	h := newTestMiddleware(t, m, func(*http.Request) string { return "" }, Limit{N: 1, Per: time.Minute})(okHandler(&n))
	do(h, "GET", "/", "203.0.113.7:1", nil)
	if do(h, "GET", "/", "198.51.100.1:1", nil).Code != 429 {
		t.Fatal("unidentified clients share one bucket")
	}
}

type failing struct{}

func (failing) Allow(context.Context, string, Limit) (Decision, error) {
	return Decision{}, errors.New("database password=hunter2 unreachable")
}

func TestMiddlewareFailsClosedByDefaultAndOpenOnRequest(t *testing.T) {
	var n atomic.Int32
	closed := newTestMiddleware(t, failing{}, ByRemoteAddr, Limit{N: 1, Per: time.Minute})(okHandler(&n))
	w := do(closed, "GET", "/", "203.0.113.7:1", nil)
	if w.Code != http.StatusServiceUnavailable || n.Load() != 0 {
		t.Fatalf("closed: %d, handler runs %d; want 503 and none", w.Code, n.Load())
	}
	if strings.Contains(w.Body.String(), "hunter2") {
		t.Fatal("limiter error details must not reach the client")
	}
	open := newTestMiddleware(t, failing{}, ByRemoteAddr, Limit{N: 1, Per: time.Minute}, FailOpen())(okHandler(&n))
	if w := do(open, "GET", "/", "203.0.113.7:1", nil); w.Code != 204 || n.Load() != 1 {
		t.Fatalf("open: %d, handler runs %d; want 204 and 1", w.Code, n.Load())
	}
}

func TestMiddlewareValidation(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	ok := Limit{N: 1, Per: time.Second}
	if _, err := Middleware(nil, ByRemoteAddr, ok); err == nil {
		t.Error("nil limiter accepted")
	}
	if _, err := Middleware(m, nil, ok); err == nil {
		t.Error("nil key function accepted")
	}
	if _, err := Middleware(m, ByRemoteAddr, Limit{}); err == nil {
		t.Error("zero limit accepted")
	}
	if _, err := Middleware(m, ByRemoteAddr, ok, WithScope("")); err == nil {
		t.Error("empty scope accepted")
	}
}

func TestMiddlewareKeysAreHashedBeforeTheLimiter(t *testing.T) {
	var seen []string
	rec := limiterFunc(func(_ context.Context, key string, _ Limit) (Decision, error) {
		seen = append(seen, key)
		return Decision{Allowed: true}, nil
	})
	var n atomic.Int32
	h := newTestMiddleware(t, rec, func(*http.Request) string { return "alice@example.com" }, Limit{N: 1, Per: time.Minute})(okHandler(&n))
	do(h, "GET", "/", "203.0.113.7:1", nil)
	if len(seen) != 1 || len(seen[0]) != 64 || strings.Contains(seen[0], "alice") {
		t.Fatalf("limiter saw %q, want a 64-character digest without the identifier", seen)
	}
}

type limiterFunc func(context.Context, string, Limit) (Decision, error)

func (f limiterFunc) Allow(ctx context.Context, key string, limit Limit) (Decision, error) {
	return f(ctx, key, limit)
}

func TestByClientIPWithoutResolverIsThePeer(t *testing.T) {
	m, _ := NewMemory(MemoryOptions{})
	var n atomic.Int32
	h := newTestMiddleware(t, m, ByClientIP, Limit{N: 1, Per: time.Minute})(okHandler(&n))
	spoof := func(ip string) http.Header {
		return http.Header{"X-Forwarded-For": {ip}, "X-Real-Ip": {ip}, "Forwarded": {"for=" + ip}}
	}
	if do(h, "GET", "/", "203.0.113.7:1", spoof("1.1.1.1")).Code != 204 {
		t.Fatal("first request")
	}
	if do(h, "GET", "/", "203.0.113.7:2", spoof("2.2.2.2")).Code != 429 {
		t.Fatal("rotating forwarded headers must not evade the limit")
	}
}

func TestByClientIPBehindTrustedProxy(t *testing.T) {
	trusted, err := web.NewTrustedProxies(netip.MustParsePrefix("172.18.0.0/16"))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := NewMemory(MemoryOptions{})
	var n atomic.Int32
	h := trusted.Middleware(newTestMiddleware(t, m, ByClientIP, Limit{N: 1, Per: time.Minute})(okHandler(&n)))
	xff := func(ip string) http.Header { return http.Header{"X-Forwarded-For": {ip}} }
	const proxy = "172.18.0.2:41000"
	if do(h, "GET", "/", proxy, xff("203.0.113.10")).Code != 204 {
		t.Fatal("client A first request")
	}
	if do(h, "GET", "/", proxy, xff("203.0.113.20")).Code != 204 {
		t.Fatal("client B behind the same proxy shares A's bucket")
	}
	if do(h, "GET", "/", proxy, xff("198.51.100.1, 203.0.113.10")).Code != 429 {
		t.Fatal("client A escaped its bucket with a forged prefix")
	}
	if do(h, "GET", "/", "203.0.113.10:5", xff("198.51.100.2")).Code != 429 {
		t.Fatal("client A escaped its bucket with a forged header sent directly")
	}
	if do(h, "GET", "/", proxy, xff("2001:db8:1:2::1")).Code != 204 {
		t.Fatal("IPv6 client first request")
	}
	if do(h, "GET", "/", proxy, xff("2001:db8:1:2::2")).Code != 429 {
		t.Fatal("IPv6 client escaped with another address of its /64")
	}
	// ByRemoteAddr keeps keying by the peer: everyone behind the proxy is
	// one bucket.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = proxy
	r.Header.Set("X-Forwarded-For", "203.0.113.10")
	trusted.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if got := ByRemoteAddr(r); got != "172.18.0.2" {
			t.Errorf("ByRemoteAddr = %q, want the peer", got)
		}
		if got := ByClientIP(r); got != "203.0.113.10" {
			t.Errorf("ByClientIP = %q, want the client", got)
		}
	})).ServeHTTP(httptest.NewRecorder(), r)
}
