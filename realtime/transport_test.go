package realtime_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fabriciobonjorno/forge-go/realtime"
	"github.com/fabriciobonjorno/forge-go/uuid"
)

// memoryHub connects any number of memoryTransports like a message bus. Like
// PostgreSQL NOTIFY it echoes a publisher's own envelopes back to it.
type memoryHub struct {
	mu      sync.Mutex
	members []*memoryTransport
}

type memoryTransport struct {
	hub      *memoryHub
	limit    int
	ready    chan struct{}
	receiver realtime.Receiver
	failing  bool
}

// get waits for the broker to start the transport and returns its receiver.
func (m *memoryTransport) get() realtime.Receiver {
	<-m.ready
	return m.receiver
}

func (h *memoryHub) transport(limit int) *memoryTransport {
	m := &memoryTransport{hub: h, limit: limit, ready: make(chan struct{})}
	h.mu.Lock()
	h.members = append(h.members, m)
	h.mu.Unlock()
	return m
}

func (m *memoryTransport) MaxPayloadBytes() int { return m.limit }

func (m *memoryTransport) Publish(_ context.Context, envelope realtime.Envelope) error {
	if m.failing {
		return errors.New("bus unavailable")
	}
	m.hub.mu.Lock()
	members := append([]*memoryTransport(nil), m.hub.members...)
	m.hub.mu.Unlock()
	for _, member := range members {
		select {
		case <-member.ready:
			_ = member.receiver.Deliver(envelope)
		default:
		}
	}
	return nil
}

func (m *memoryTransport) Run(ctx context.Context, receiver realtime.Receiver) error {
	m.receiver = receiver
	close(m.ready)
	<-ctx.Done()
	return nil
}

func (m *memoryTransport) gap() {
	m.get().Gap()
}

func TestTransportSharesEventsBetweenBrokersWithoutCrossingTenants(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenantA, tenantB := newTenant(t), newTenant(t)
	hub := &memoryHub{}
	channels := []realtime.Channel{ordersRead, {Name: "announcements", Permission: "announcements:read", Global: true}}
	t1, t2 := hub.transport(1<<20), hub.transport(1<<20)
	one := newBroker(t, realtime.Options{Channels: channels, Transport: t1})
	two := newBroker(t, realtime.Options{Channels: channels, Transport: t2})
	t1.get()
	t2.get()

	onA := connect(t, one, d, d.add(t, tenantA, "orders:read", "announcements:read"), "orders", "announcements")
	onB := connect(t, one, d, d.add(t, tenantB, "orders:read", "announcements:read"), "orders", "announcements")
	twoA := connect(t, two, d, d.add(t, tenantA, "orders:read", "announcements:read"), "orders", "announcements")
	twoB := connect(t, two, d, d.add(t, tenantB, "orders:read", "announcements:read"), "orders", "announcements")

	// Tenant A publishes on replica two: A's subscribers on both replicas
	// receive it exactly once; tenant B's receive nothing.
	ctx := d.context(t, d.add(t, tenantA))
	sent, err := two.Publish(ctx, "orders", "order.created", "hello")
	if err != nil {
		t.Fatal(err)
	}
	for name, conn := range map[string]*realtime.Conn{"replica one": onA, "replica two": twoA} {
		if got := next(t, conn); got.ID != sent.ID || string(got.Data) != `"hello"` {
			t.Fatalf("%s: got %+v", name, got)
		}
		expectNone(t, conn, 50*time.Millisecond) // no echo duplicate
	}
	expectNone(t, onB, 50*time.Millisecond)
	expectNone(t, twoB, 50*time.Millisecond)

	// Global events reach every tenant on every replica.
	if _, err := one.Publish(ctx, "announcements", "notice.posted", "all", realtime.Global()); err != nil {
		t.Fatal(err)
	}
	for _, conn := range []*realtime.Conn{onA, onB, twoA, twoB} {
		if got := next(t, conn); string(got.Data) != `"all"` {
			t.Fatalf("global: %s", got.Data)
		}
	}
	// Each broker accepted only what the other published.
	if two.Stats().Received != 1 || one.Stats().Received != 1 {
		t.Fatalf("received one=%d two=%d", one.Stats().Received, two.Stats().Received)
	}
}

func TestTransportFailureStillServesLocalSubscribers(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)
	hub := &memoryHub{}
	transport := hub.transport(1 << 20)
	broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, Transport: transport})
	conn := connect(t, broker, d, d.add(t, tenant, "orders:read"), "orders")

	transport.failing = true
	event, err := broker.Publish(d.context(t, d.add(t, tenant)), "orders", "order.created", 1)
	if !errors.Is(err, realtime.ErrTransport) {
		t.Fatalf("err=%v, want ErrTransport", err)
	}
	if got := next(t, conn); got.ID != event.ID {
		t.Fatalf("local subscriber missed the event: %+v", got)
	}
}

func TestTransportPayloadLimitAppliesBeforeAnyDelivery(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)
	hub := &memoryHub{}
	broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, Transport: hub.transport(40)})
	conn := connect(t, broker, d, d.add(t, tenant, "orders:read"), "orders")
	big := make([]byte, 100)
	for i := range big {
		big[i] = 'x'
	}
	_, err := broker.Publish(d.context(t, d.add(t, tenant)), "orders", "order.created", string(big))
	if !errors.Is(err, realtime.ErrPayloadTooLarge) {
		t.Fatalf("err=%v", err)
	}
	expectNone(t, conn, 50*time.Millisecond) // replicas never disagree about what was published
}

func TestTransportGapTellsSubscribersToResync(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)
	hub := &memoryHub{}
	transport := hub.transport(1 << 20)
	broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, Transport: transport})
	u := d.add(t, tenant, "orders:read")
	conn := connect(t, broker, d, u, "orders")
	before, err := broker.Publish(d.context(t, d.add(t, tenant)), "orders", "order.created", 1)
	if err != nil {
		t.Fatal(err)
	}
	next(t, conn)

	transport.gap()
	got := nextMatching(t, conn, func(e realtime.Event) bool { return e.Type == realtime.EventResync })
	if got.Topic != "orders" {
		t.Fatalf("resync topic=%q", got.Topic)
	}
	// A client resuming from before the gap is told too.
	ctx := d.context(t, u)
	resumed, err := broker.NewConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resumed.Close)
	if err := resumed.Subscribe(ctx, "orders", realtime.After(before.ID)); err != nil {
		t.Fatal(err)
	}
	nextMatching(t, resumed, func(e realtime.Event) bool { return e.Type == realtime.EventResync })
	if broker.Stats().Gaps != 1 {
		t.Fatalf("gaps=%d", broker.Stats().Gaps)
	}
}

func TestBrokerRejectsForgedEnvelopes(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)
	hub := &memoryHub{}
	transport := hub.transport(1 << 20)
	broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, Transport: transport, MaxPayloadBytes: 64})
	conn := connect(t, broker, d, d.add(t, tenant, "orders:read"), "orders")
	receiver := transport.get()

	valid := realtime.Event{ID: uuid.MustNew(), Topic: "orders", Type: "order.created", Time: time.Now(), Data: []byte(`1`)}
	with := func(mutate func(*realtime.Envelope)) realtime.Envelope {
		envelope := realtime.Envelope{Origin: uuid.MustNew(), Scope: tenant.ID.String(), Event: valid}
		mutate(&envelope)
		return envelope
	}
	forged := map[string]realtime.Envelope{
		"unknown scope":     with(func(e *realtime.Envelope) { e.Scope = "everyone" }),
		"scope not a v7":    with(func(e *realtime.Envelope) { e.Scope = "00000000-0000-4000-8000-000000000000" }),
		"bad topic":         with(func(e *realtime.Envelope) { e.Event.Topic = "ORDERS" }),
		"synthetic type":    with(func(e *realtime.Envelope) { e.Event.Type = realtime.EventResync }),
		"bad type":          with(func(e *realtime.Envelope) { e.Event.Type = "a\nb" }),
		"zero event id":     with(func(e *realtime.Envelope) { e.Event.ID = uuid.UUID{} }),
		"oversized payload": with(func(e *realtime.Envelope) { e.Event.Data = make([]byte, 65) }),
		"invalid payload":   with(func(e *realtime.Envelope) { e.Event.Data = []byte(`{`) }),
	}
	for name, envelope := range forged {
		if err := receiver.Deliver(envelope); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	expectNone(t, conn, 50*time.Millisecond)
	if err := receiver.Deliver(with(func(*realtime.Envelope) {})); err != nil {
		t.Fatalf("valid envelope: %v", err)
	}
	if got := next(t, conn); got.ID != valid.ID {
		t.Fatalf("got %+v", got)
	}
	if got := broker.Stats().Rejected; got != uint64(len(forged)) {
		t.Fatalf("rejected=%d, want %d", got, len(forged))
	}
}

var tlsState tls.ConnectionState

func TestOriginChecker(t *testing.T) {
	t.Parallel()
	request := func(host, origin string, tls bool) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/events", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if tls {
			r.TLS = &tlsState
		}
		return r
	}
	checker, err := realtime.NewOriginChecker(realtime.OriginPolicy{Allowed: []string{"https://app.example.com", "HTTP://Admin.Example.com:8080"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		host   string
		origin string
		tls    bool
		allow  bool
	}{
		{"same origin", "example.com", "https://example.com", true, true},
		{"same host different case", "Example.com", "http://example.COM", false, true},
		{"same host with port", "localhost:8080", "http://localhost:8080", false, true},
		{"other port", "localhost:8080", "http://localhost:9090", false, false},
		{"cross site", "example.com", "https://evil.example.net", true, false},
		{"suffix trick", "example.com", "https://example.com.evil.net", true, false},
		{"prefix trick", "example.com", "https://evilexample.com", true, false},
		{"downgrade over TLS", "example.com", "http://example.com", true, false},
		{"allowlisted", "example.com", "https://app.example.com", true, true},
		{"allowlisted normalized", "example.com", "http://admin.example.com:8080", false, true},
		{"allowlist is exact", "example.com", "https://app.example.com:444", true, false},
		{"null origin", "example.com", "null", true, false},
		{"file scheme", "example.com", "file://example.com", true, false},
		{"origin with path", "example.com", "https://example.com/x", true, false},
		{"no origin (non-browser)", "example.com", "", true, true},
	}
	for _, c := range cases {
		err := checker.Check(request(c.host, c.origin, c.tls))
		if c.allow != (err == nil) {
			t.Errorf("%s: err=%v, want allow=%v", c.name, err, c.allow)
		}
	}
	strict, err := realtime.NewOriginChecker(realtime.OriginPolicy{RequireOrigin: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := strict.Check(request("example.com", "", true)); !errors.Is(err, realtime.ErrOriginDenied) {
		t.Errorf("RequireOrigin without Origin: %v", err)
	}
	two := request("example.com", "https://example.com", true)
	two.Header.Add("Origin", "https://example.com")
	if err := checker.Check(two); err == nil {
		t.Error("two Origin headers were accepted")
	}
	for _, bad := range []string{"example.com", "https://example.com/path", "ftp://example.com", ""} {
		if _, err := realtime.NewOriginChecker(realtime.OriginPolicy{Allowed: []string{bad}}); err == nil {
			t.Errorf("allowed origin %q was accepted", bad)
		}
	}
}
