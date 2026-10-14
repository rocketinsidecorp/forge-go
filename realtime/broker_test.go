package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fabriciobonjorno/forge-go/auth"
	"github.com/fabriciobonjorno/forge-go/realtime"
	"github.com/fabriciobonjorno/forge-go/tenancy"
	"github.com/fabriciobonjorno/forge-go/uuid"
)

var ordersRead = realtime.Channel{Name: "orders", Permission: "orders:read"}

func TestFanOutDeliversToEverySubscriberInOrder(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)
	broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, MaxConns: 1000, MaxConnsPerSubject: 1000})

	const subscribers, events = 300, 25
	conns := make([]*realtime.Conn, subscribers)
	for i := range conns {
		conns[i] = connect(t, broker, d, d.add(t, tenant, "orders:read"), "orders")
	}
	publisher := d.context(t, d.add(t, tenant))
	for i := range events {
		if _, err := broker.Publish(publisher, "orders", "order.created", map[string]int{"n": i}); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for _, conn := range conns {
		wg.Go(func() {
			var last uuid.UUID
			for want := range events {
				event := next(t, conn)
				var payload map[string]int
				if err := json.Unmarshal(event.Data, &payload); err != nil || payload["n"] != want {
					t.Errorf("event %d payload=%s err=%v", want, event.Data, err)
					return
				}
				if event.ID.Compare(last) <= 0 || event.Type != "order.created" || event.Topic != "orders" {
					t.Errorf("event %d: id=%v type=%q topic=%q", want, event.ID, event.Type, event.Topic)
					return
				}
				last = event.ID
			}
		})
	}
	wg.Wait()
	if stats := broker.Stats(); stats.Conns != subscribers || stats.Subscriptions != subscribers || stats.Published != events {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestSlowConsumerIsDisconnectedWithoutBlockingOthers(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)
	broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, QueueSize: 8})

	slow := connect(t, broker, d, d.add(t, tenant, "orders:read"), "orders")
	fast := connect(t, broker, d, d.add(t, tenant, "orders:read"), "orders")
	publisher := d.context(t, d.add(t, tenant))

	const events = 200
	var received atomic.Int64
	go func() {
		for {
			select {
			case event := <-fast.Events():
				if !event.Synthetic() {
					received.Add(1)
				}
			case <-fast.Done():
				return
			}
		}
	}()
	start := time.Now()
	for i := range events {
		if _, err := broker.Publish(publisher, "orders", "order.created", i); err != nil {
			t.Fatal(err)
		}
		// Let the fast reader keep up so that only the idle one overflows.
		for deadline := time.Now().Add(5 * time.Second); received.Load() <= int64(i); {
			if time.Now().After(deadline) {
				t.Fatalf("fast consumer stalled at %d of %d events", received.Load(), events)
			}
			runtime.Gosched()
		}
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("publishing %d events took %s; a slow consumer must not block publishers", events, elapsed)
	}
	select {
	case <-slow.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("slow consumer was not disconnected")
	}
	if !errors.Is(slow.Err(), realtime.ErrSlowConsumer) {
		t.Fatalf("slow.Err()=%v", slow.Err())
	}
	if stats := broker.Stats(); stats.SlowDisconnects != 1 || stats.Conns != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	// Memory stays bounded: the queue never holds more than its capacity.
	if len(slow.Events()) > 8 {
		t.Fatalf("slow queue length %d exceeds capacity", len(slow.Events()))
	}
}

func TestLimits(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)
	channels := []realtime.Channel{
		ordersRead,
		{Name: "room:{id}", Permission: "rooms:read"},
	}

	t.Run("connections in total", func(t *testing.T) {
		t.Parallel()
		broker := newBroker(t, realtime.Options{Channels: channels, MaxConns: 2})
		connect(t, broker, d, d.add(t, tenant))
		connect(t, broker, d, d.add(t, tenant))
		_, err := broker.NewConn(d.context(t, d.add(t, tenant)))
		if !errors.Is(err, realtime.ErrLimitReached) {
			t.Fatalf("third connection: %v", err)
		}
	})

	t.Run("connections per subject, released on close", func(t *testing.T) {
		t.Parallel()
		broker := newBroker(t, realtime.Options{Channels: channels, MaxConnsPerSubject: 2})
		u := d.add(t, tenant)
		first := connect(t, broker, d, u)
		connect(t, broker, d, u)
		if _, err := broker.NewConn(d.context(t, u)); !errors.Is(err, realtime.ErrLimitReached) {
			t.Fatalf("third connection for one subject: %v", err)
		}
		first.Close()
		again, err := broker.NewConn(d.context(t, u))
		if err != nil {
			t.Fatalf("connection after a close: %v", err)
		}
		again.Close()
		// Another subject is unaffected.
		connect(t, broker, d, d.add(t, tenant))
	})

	t.Run("subscriptions per connection", func(t *testing.T) {
		t.Parallel()
		broker := newBroker(t, realtime.Options{Channels: channels, MaxSubscriptionsPerConn: 2})
		u := d.add(t, tenant, "rooms:read")
		conn := connect(t, broker, d, u, "room:1", "room:2")
		if err := conn.Subscribe(d.context(t, u), "room:3"); !errors.Is(err, realtime.ErrLimitReached) {
			t.Fatalf("third subscription: %v", err)
		}
		// Repeating an existing subscription is not a new one.
		if err := conn.Subscribe(d.context(t, u), "room:1"); err != nil {
			t.Fatalf("repeat subscription: %v", err)
		}
		conn.Unsubscribe("room:1")
		if err := conn.Subscribe(d.context(t, u), "room:3"); err != nil {
			t.Fatalf("subscription after unsubscribe: %v", err)
		}
	})

	t.Run("subscriptions in total", func(t *testing.T) {
		t.Parallel()
		broker := newBroker(t, realtime.Options{Channels: channels, MaxSubscriptions: 3})
		u := d.add(t, tenant, "rooms:read")
		connect(t, broker, d, u, "room:1", "room:2")
		other := connect(t, broker, d, u, "room:1")
		if err := other.Subscribe(d.context(t, u), "room:2"); !errors.Is(err, realtime.ErrLimitReached) {
			t.Fatalf("subscription over the global limit: %v", err)
		}
		other.Close()
		if got := broker.Stats().Subscriptions; got != 2 {
			t.Fatalf("subscriptions after close=%d", got)
		}
	})

	t.Run("payload size and validity", func(t *testing.T) {
		t.Parallel()
		broker := newBroker(t, realtime.Options{Channels: channels, MaxPayloadBytes: 32})
		ctx := d.context(t, d.add(t, tenant))
		if _, err := broker.Publish(ctx, "orders", "order.created", strings.Repeat("x", 64)); !errors.Is(err, realtime.ErrPayloadTooLarge) {
			t.Fatalf("oversized payload: %v", err)
		}
		if _, err := broker.Publish(ctx, "orders", "order.created", make(chan int)); !errors.Is(err, realtime.ErrPayloadInvalid) {
			t.Fatalf("unencodable payload: %v", err)
		}
		if _, err := broker.Publish(ctx, "Orders", "order.created", 1); !errors.Is(err, realtime.ErrTopicInvalid) {
			t.Fatalf("invalid topic: %v", err)
		}
		for _, bad := range []string{"", "Order", "forge.resync", "a b", "line\nbreak"} {
			if _, err := broker.Publish(ctx, "orders", bad, 1); !errors.Is(err, realtime.ErrEventTypeInvalid) {
				t.Fatalf("event type %q: %v", bad, err)
			}
		}
	})
}

func TestTenantIsolation(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenantA, tenantB := newTenant(t), newTenant(t)
	broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{
		ordersRead,
		{Name: "announcements", Permission: "announcements:read", Global: true},
	}})

	a := connect(t, broker, d, d.add(t, tenantA, "orders:read", "announcements:read"), "orders", "announcements")
	b := connect(t, broker, d, d.add(t, tenantB, "orders:read", "announcements:read"), "orders", "announcements")
	ctxA := d.context(t, d.add(t, tenantA))
	ctxB := d.context(t, d.add(t, tenantB))

	if _, err := broker.Publish(ctxA, "orders", "order.created", "for A"); err != nil {
		t.Fatal(err)
	}
	if got := next(t, a); string(got.Data) != `"for A"` {
		t.Fatalf("A got %s", got.Data)
	}
	expectNone(t, b, 100*time.Millisecond)

	if _, err := broker.Publish(ctxB, "orders", "order.created", "for B"); err != nil {
		t.Fatal(err)
	}
	if got := next(t, b); string(got.Data) != `"for B"` {
		t.Fatalf("B got %s", got.Data)
	}
	expectNone(t, a, 100*time.Millisecond)

	// ForTenant addresses a tenant without a request context (a job).
	if _, err := broker.Publish(context.Background(), "orders", "order.created", "job for A", realtime.ForTenant(tenantA)); err != nil {
		t.Fatal(err)
	}
	if got := next(t, a); string(got.Data) != `"job for A"` {
		t.Fatalf("A got %s", got.Data)
	}
	expectNone(t, b, 100*time.Millisecond)

	// A tenant publish never reaches a global channel with the same name, and
	// a global publish reaches every tenant.
	if _, err := broker.Publish(ctxA, "announcements", "notice.posted", "tenant-local"); err != nil {
		t.Fatal(err)
	}
	expectNone(t, a, 100*time.Millisecond)
	expectNone(t, b, 50*time.Millisecond)
	if _, err := broker.Publish(ctxA, "announcements", "notice.posted", "everyone", realtime.Global()); err != nil {
		t.Fatal(err)
	}
	for _, conn := range []*realtime.Conn{a, b} {
		if got := next(t, conn); string(got.Data) != `"everyone"` {
			t.Fatalf("global event=%s", got.Data)
		}
	}

	// Publishing without a tenant fails closed.
	if _, err := broker.Publish(context.Background(), "orders", "order.created", 1); !errors.Is(err, tenancy.ErrMissing) {
		t.Fatalf("publish without tenant: %v", err)
	}
	if _, err := broker.Publish(ctxA, "orders", "order.created", 1, realtime.Global(), realtime.ForTenant(tenantB)); err == nil {
		t.Fatal("Global and ForTenant together must be rejected")
	}
}

func TestSubscriptionIsDeniedByDefault(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)
	var hookParams realtime.Params
	broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{
		ordersRead,
		{Name: "room:{id}", Permission: "rooms:read", Authorize: func(_ context.Context, p auth.Principal, params realtime.Params) error {
			hookParams = params
			if params["id"] == "secret" {
				return errors.New("not your room")
			}
			if p.Tenant().ID == (uuid.UUID{}) {
				return errors.New("no tenant")
			}
			return nil
		}},
		{Name: "room:admin", Permission: "admin:read"},
	}})

	reader := d.add(t, tenant, "orders:read", "rooms:read")
	ctx := d.context(t, reader)
	conn, err := broker.NewConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)

	denied := map[string]string{
		"undeclared":    "unknown",
		"other segment": "orders:extra",
		"hook refuses":  "room:secret",
		"most specific": "room:admin", // matches the admin channel, not room:{id}
	}
	for name, topic := range denied {
		err := conn.Subscribe(ctx, topic)
		if !errors.Is(err, realtime.ErrSubscriptionDenied) {
			t.Errorf("%s (%s): err=%v", name, topic, err)
		}
	}
	// The same error whether the channel exists or not: no probing.
	e1, e2 := conn.Subscribe(ctx, "unknown"), conn.Subscribe(ctx, "room:admin")
	if e1.Error() != e2.Error() {
		t.Errorf("distinguishable denials: %q vs %q", e1, e2)
	}
	if err := conn.Subscribe(ctx, "Not A Topic"); !errors.Is(err, realtime.ErrTopicInvalid) {
		t.Errorf("invalid topic: %v", err)
	}
	if err := conn.Subscribe(ctx, "orders"); err != nil {
		t.Errorf("permitted orders: %v", err)
	}
	if err := conn.Subscribe(ctx, "room:lobby"); err != nil {
		t.Errorf("permitted room: %v", err)
	}
	if hookParams["id"] != "lobby" {
		t.Errorf("hook params=%v", hookParams)
	}

	// No permissions at all: nothing is allowed.
	nobody := d.add(t, tenant)
	nctx := d.context(t, nobody)
	nconn, err := broker.NewConn(nctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nconn.Close)
	for _, topic := range []string{"orders", "room:lobby"} {
		if err := nconn.Subscribe(nctx, topic); !errors.Is(err, realtime.ErrSubscriptionDenied) {
			t.Errorf("principal without permissions subscribed to %q: %v", topic, err)
		}
	}

	// A connection needs an authenticated principal.
	if _, err := broker.NewConn(context.Background()); !errors.Is(err, realtime.ErrUnauthenticated) {
		t.Errorf("unauthenticated NewConn: %v", err)
	}
	if got := broker.Stats().Subscriptions; got != 2 {
		t.Errorf("subscriptions=%d, want only the 2 authorized ones", got)
	}
}

func TestChannelRegistrationRejectsUnsafeDeclarations(t *testing.T) {
	t.Parallel()
	broker := newBroker(t, realtime.Options{})
	bad := map[string]realtime.Channel{
		"no permission":  {Name: "orders"},
		"bad permission": {Name: "orders", Permission: "ORDERS"},
		"uppercase":      {Name: "Orders", Permission: "orders:read"},
		"empty segment":  {Name: "room::x", Permission: "rooms:read"},
		"bad param":      {Name: "room:{Id}", Permission: "rooms:read"},
		"half brace":     {Name: "room:{id", Permission: "rooms:read"},
		"repeated param": {Name: "a:{id}:{id}", Permission: "rooms:read"},
	}
	for name, channel := range bad {
		if err := broker.Register(channel); err == nil {
			t.Errorf("%s: channel %+v was accepted", name, channel)
		}
	}
	if err := broker.Register(realtime.Channel{Name: "room:{id}", Permission: "rooms:read"}); err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(realtime.Channel{Name: "room:{other}", Permission: "rooms:write"}); err == nil {
		t.Error("ambiguous channel shapes were accepted")
	}
}

func TestReplayResumesFromLastEventID(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)
	broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}})
	reader := d.add(t, tenant, "orders:read")
	ctx := d.context(t, reader)
	publisher := d.context(t, d.add(t, tenant))

	var ids []uuid.UUID
	for i := range 5 {
		event, err := broker.Publish(publisher, "orders", "order.created", i)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, event.ID)
	}
	if _, err := broker.Publish(publisher, "orders", "order.typing", "x", realtime.Ephemeral()); err != nil {
		t.Fatal(err)
	}

	// Resuming after the second event replays the next three, in order, and
	// reports no gap (the retained window covers everything newer).
	conn, err := broker.NewConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	if err := conn.Subscribe(ctx, "orders", realtime.After(ids[1])); err != nil {
		t.Fatal(err)
	}
	first := <-conn.Events()
	if first.Type != realtime.EventSubscribed {
		t.Fatalf("first event %q, want the subscription acknowledgement", first.Type)
	}
	for _, want := range ids[2:] {
		if got := next(t, conn); got.ID != want {
			t.Fatalf("replayed %v, want %v", got.ID, want)
		}
	}
	expectNone(t, conn, 50*time.Millisecond) // the ephemeral event was not retained

	// Another tenant's replay window never leaks.
	other := d.add(t, newTenant(t), "orders:read")
	octx := d.context(t, other)
	oconn, err := broker.NewConn(octx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oconn.Close)
	if err := oconn.Subscribe(octx, "orders", realtime.After(ids[0])); err != nil {
		t.Fatal(err)
	}
	for _, event := range drain(oconn) {
		if event.Type != realtime.EventSubscribed && event.Type != realtime.EventResync {
			t.Fatalf("tenant leak through replay: %+v", event)
		}
	}

	// Live events continue after the replay without duplicates.
	live, err := broker.Publish(publisher, "orders", "order.created", "live")
	if err != nil {
		t.Fatal(err)
	}
	if got := next(t, conn); got.ID != live.ID {
		t.Fatalf("live event %v, want %v", got.ID, live.ID)
	}
}

func TestReplayReportsGapsInsteadOfSilentLoss(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)
	reader := d.add(t, tenant, "orders:read")
	ctx := d.context(t, reader)
	publisher := d.context(t, d.add(t, tenant))

	resync := func(conn *realtime.Conn) bool {
		for _, event := range drain(conn) {
			if event.Type == realtime.EventResync && event.Topic == "orders" {
				return true
			}
		}
		return false
	}
	subscribeAfter := func(broker *realtime.Broker, after uuid.UUID) *realtime.Conn {
		conn, err := broker.NewConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(conn.Close)
		if err := conn.Subscribe(ctx, "orders", realtime.After(after)); err != nil {
			t.Fatal(err)
		}
		return conn
	}

	t.Run("evicted by count", func(t *testing.T) {
		t.Parallel()
		broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, ReplayEvents: 3})
		var ids []uuid.UUID
		for i := range 6 {
			event, _ := broker.Publish(publisher, "orders", "order.created", i)
			ids = append(ids, event.ID)
		}
		if !resync(subscribeAfter(broker, ids[0])) {
			t.Error("resuming from an evicted event must report a gap")
		}
		if resync(subscribeAfter(broker, ids[4])) {
			t.Error("resuming inside the window must not report a gap")
		}
		if got := broker.Stats().ReplayEvents; got != 3 {
			t.Errorf("retained %d events, want 3", got)
		}
	})

	t.Run("bounded by bytes", func(t *testing.T) {
		t.Parallel()
		broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, ReplayBytes: 1024})
		payload := strings.Repeat("x", 300)
		for range 50 {
			if _, err := broker.Publish(publisher, "orders", "order.created", payload); err != nil {
				t.Fatal(err)
			}
		}
		if got := broker.Stats().ReplayEvents; got > 3 {
			t.Errorf("retained %d events of ~400 bytes under a 1 KiB cap", got)
		}
	})

	t.Run("expired by age", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		now := time.Now()
		clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, ReplayTTL: time.Minute, Clock: clock})
		first, _ := broker.Publish(publisher, "orders", "order.created", 1)
		if _, err := broker.Publish(publisher, "orders", "order.created", 2); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		now = now.Add(2 * time.Minute)
		mu.Unlock()
		if _, err := broker.Publish(publisher, "orders", "order.created", 3); err != nil {
			t.Fatal(err)
		}
		if !resync(subscribeAfter(broker, first.ID)) {
			t.Error("an expired event newer than the client's last must report a gap")
		}
	})

	t.Run("event older than this process", func(t *testing.T) {
		t.Parallel()
		broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}})
		// A UUIDv7 from before the broker started: the client was connected to
		// a previous process or another replica.
		before, err := uuid.NewGenerator(func() time.Time { return time.Now().Add(-time.Hour) }, zeros{})
		if err != nil {
			t.Fatal(err)
		}
		old, _ := before.New()
		if !resync(subscribeAfter(broker, old)) {
			t.Error("an ID from before this process started must report a gap")
		}
	})

	t.Run("replay disabled", func(t *testing.T) {
		t.Parallel()
		broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, ReplayEvents: -1})
		event, _ := broker.Publish(publisher, "orders", "order.created", 1)
		if !resync(subscribeAfter(broker, event.ID)) {
			t.Error("with replay disabled every resume must report a gap")
		}
	})
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func drain(conn *realtime.Conn) []realtime.Event {
	var events []realtime.Event
	for {
		select {
		case event := <-conn.Events():
			events = append(events, event)
		default:
			return events
		}
	}
}

func TestConnectionLifetimeAndRevalidation(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)

	t.Run("lifetime", func(t *testing.T) {
		t.Parallel()
		broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, MaxConnLifetime: 50 * time.Millisecond})
		conn := connect(t, broker, d, d.add(t, tenant, "orders:read"), "orders")
		select {
		case <-conn.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("connection outlived its maximum lifetime")
		}
		if !errors.Is(conn.Err(), realtime.ErrSessionExpired) {
			t.Fatalf("Err()=%v", conn.Err())
		}
	})

	t.Run("revalidate", func(t *testing.T) {
		t.Parallel()
		var revoked sync.Map
		broker := newBroker(t, realtime.Options{
			Channels:           []realtime.Channel{ordersRead},
			RevalidateInterval: 20 * time.Millisecond,
			Revalidate: func(_ context.Context, p auth.Principal) error {
				if _, gone := revoked.Load(p.SubjectID()); gone {
					return errors.New("session revoked")
				}
				return nil
			},
		})
		u := d.add(t, tenant, "orders:read")
		conn := connect(t, broker, d, u, "orders")
		other := connect(t, broker, d, d.add(t, tenant, "orders:read"), "orders")
		time.Sleep(100 * time.Millisecond)
		select {
		case <-conn.Done():
			t.Fatal("connection closed before revocation")
		default:
		}
		revoked.Store(u.principal.SubjectID(), true)
		select {
		case <-conn.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("revoked session stayed connected")
		}
		if !errors.Is(conn.Err(), realtime.ErrSessionExpired) {
			t.Fatalf("Err()=%v", conn.Err())
		}
		select {
		case <-other.Done():
			t.Fatal("unrelated connection was closed")
		default:
		}
	})

	t.Run("context", func(t *testing.T) {
		t.Parallel()
		broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}})
		u := d.add(t, tenant, "orders:read")
		ctx, cancel := context.WithCancel(d.context(t, u))
		conn, err := broker.NewConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case <-conn.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("connection outlived its request context")
		}
		if broker.Stats().Conns != 0 {
			t.Fatal("connection not released")
		}
	})
}

func TestShutdownClosesEverything(t *testing.T) {
	d := newDirectory()
	tenant := newTenant(t)
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	broker, err := realtime.New(ctx, realtime.Options{Logger: discard, Channels: []realtime.Channel{ordersRead}})
	if err != nil {
		t.Fatal(err)
	}
	var conns []*realtime.Conn
	for range 20 {
		u := d.add(t, tenant, "orders:read")
		reqCtx := d.context(t, u)
		conn, err := broker.NewConn(reqCtx)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Subscribe(reqCtx, "orders"); err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}

	// Cancelling the base context (the process signal context) closes streams
	// without waiting for the explicit Close.
	cancel()
	for _, conn := range conns {
		select {
		case <-conn.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("connection still open after the broker context ended")
		}
		if !errors.Is(conn.Err(), realtime.ErrClosed) {
			t.Fatalf("Err()=%v", conn.Err())
		}
	}
	if err := broker.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := broker.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	reqCtx := d.context(t, d.add(t, tenant, "orders:read"))
	if _, err := broker.NewConn(reqCtx); !errors.Is(err, realtime.ErrClosed) {
		t.Fatalf("NewConn after close: %v", err)
	}
	if _, err := broker.Publish(reqCtx, "orders", "order.created", 1); !errors.Is(err, realtime.ErrClosed) {
		t.Fatalf("Publish after close: %v", err)
	}
	if stats := broker.Stats(); stats.Conns != 0 || stats.Subscriptions != 0 {
		t.Fatalf("stats after close=%+v", stats)
	}
	settleGoroutines(t, before)
}

func TestConnectionChurnDoesNotLeakGoroutines(t *testing.T) {
	d := newDirectory()
	tenant := newTenant(t)
	broker := newBroker(t, realtime.Options{
		Channels: []realtime.Channel{ordersRead}, MaxConns: 5000, MaxConnsPerSubject: 5000,
		RevalidateInterval: time.Hour, Revalidate: func(context.Context, auth.Principal) error { return nil },
	})
	u := d.add(t, tenant, "orders:read")
	reqCtx := d.context(t, u)
	before := runtime.NumGoroutine()
	for range 500 {
		ctx, cancel := context.WithCancel(reqCtx)
		conn, err := broker.NewConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Subscribe(ctx, "orders"); err != nil {
			t.Fatal(err)
		}
		if _, err := broker.Publish(ctx, "orders", "order.created", 1); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		cancel()
	}
	settleGoroutines(t, before+2)
	if stats := broker.Stats(); stats.Conns != 0 || stats.Subscriptions != 0 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestPublishIsSafeUnderConcurrentSubscribeAndClose(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	tenant := newTenant(t)
	broker := newBroker(t, realtime.Options{Channels: []realtime.Channel{ordersRead}, QueueSize: 16, MaxConnsPerSubject: 10000})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			ctx := d.context(t, d.add(t, tenant))
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := broker.Publish(ctx, "orders", "order.created", i); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for i := range 4 {
		wg.Go(func() {
			u := d.add(t, tenant, "orders:read")
			ctx := d.context(t, u)
			for j := 0; j < 200; j++ {
				conn, err := broker.NewConn(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Subscribe(ctx, "orders")
				if (i+j)%3 == 0 {
					drain(conn)
				}
				conn.Close()
			}
		})
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if stats := broker.Stats(); stats.Conns != 0 || stats.Subscriptions != 0 {
		t.Fatalf("stats=%+v", stats)
	}
}
