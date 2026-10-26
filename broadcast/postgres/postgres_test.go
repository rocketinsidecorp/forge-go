package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/broadcast"
	bcpostgres "github.com/rocketinsidecorp/forge-go/broadcast/postgres"
	"github.com/rocketinsidecorp/forge-go/internal/waittest"
	forgepostgres "github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newBroker(t *testing.T, db *forgepostgres.DB, opts bcpostgres.Options) *bcpostgres.Broker {
	t.Helper()
	local, err := broadcast.NewMemory(context.Background(), broadcast.MemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Logger == nil {
		opts.Logger = quiet
	}
	broker, err := bcpostgres.New(context.Background(), db, local, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = broker.Shutdown(ctx)
	})
	waitListening(t, broker)
	return broker
}

func waitListening(t *testing.T, broker *bcpostgres.Broker) {
	t.Helper()
	if !waittest.Poll(15*time.Second, broker.Listening) {
		t.Fatal("the broker never started listening")
	}
}

func global(t *testing.T, name string) broadcast.Channel {
	t.Helper()
	channel, err := broadcast.Global(name)
	if err != nil {
		t.Fatal(err)
	}
	return channel
}

func receive(t *testing.T, sub broadcast.Subscription) string {
	t.Helper()
	select {
	case message, ok := <-sub.Messages():
		if !ok {
			t.Fatalf("subscription ended: %v", sub.Err())
		}
		return message.Data
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a message")
		return ""
	}
}

func expectNothing(t *testing.T, sub broadcast.Subscription) {
	t.Helper()
	select {
	case message := <-sub.Messages():
		t.Fatalf("unexpected message %q", message.Data)
	case <-time.After(300 * time.Millisecond):
	}
}

// Two brokers on one database stand in for two replicas of an application.
func TestMessagesCrossReplicas(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	a, b := newBroker(t, db, bcpostgres.Options{}), newBroker(t, db, bcpostgres.Options{})
	ctx := context.Background()
	channel := global(t, "posts")
	onA, err := a.Subscribe(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	onB, _ := b.Subscribe(ctx, channel)
	elsewhere, _ := b.Subscribe(ctx, global(t, "comments"))

	if err := b.Publish(ctx, channel, broadcast.Message{Data: "from b"}); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, onA); got != "from b" {
		t.Fatalf("replica A got %q", got)
	}
	if got := receive(t, onB); got != "from b" {
		t.Fatalf("replica B got %q", got)
	}
	// Each subscriber hears a message exactly once: B does not deliver its own
	// notification a second time.
	expectNothing(t, onA)
	expectNothing(t, onB)
	expectNothing(t, elsewhere)
}

func TestTenantChannelsStaySeparateAcrossReplicas(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	a, b := newBroker(t, db, bcpostgres.Options{}), newBroker(t, db, bcpostgres.Options{})
	ctx := context.Background()
	tenantOne, tenantTwo := uuid.MustNew(), uuid.MustNew()
	one, _ := broadcast.ForTenant(tenantOne, "posts")
	two, _ := broadcast.ForTenant(tenantTwo, "posts")
	subOne, _ := a.Subscribe(ctx, one)
	subTwo, _ := a.Subscribe(ctx, two)
	subGlobal, _ := a.Subscribe(ctx, global(t, "posts"))

	if err := b.Publish(ctx, one, broadcast.Message{Data: "tenant one"}); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, subOne); got != "tenant one" {
		t.Fatalf("got %q", got)
	}
	expectNothing(t, subTwo)
	expectNothing(t, subGlobal)
}

func TestLargeMessagesAreChunkedAndReassembled(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	a, b := newBroker(t, db, bcpostgres.Options{}), newBroker(t, db, bcpostgres.Options{})
	ctx := context.Background()
	channel := global(t, "posts")
	sub, _ := a.Subscribe(ctx, channel)

	// Far beyond the 8000 byte NOTIFY limit, with multi-byte text, line breaks
	// and bytes a payload could mangle.
	payload := strings.Repeat(`<turbo-stream action="append" target="posts"><template><li>é 日本語 "quoted" & <b>x</b></li></template></turbo-stream>`+"\r\n\x00", 1500)
	if len(payload) < 150000 {
		t.Fatalf("payload is only %d bytes", len(payload))
	}
	if err := b.Publish(ctx, channel, broadcast.Message{Data: payload}); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, sub); got != payload {
		t.Fatalf("reassembled message differs (%d bytes, want %d)", len(got), len(payload))
	}

	// Messages published back to back keep their order, large or small.
	for i := range 5 {
		if err := b.Publish(ctx, channel, broadcast.Message{Data: fmt.Sprintf("%d:%s", i, strings.Repeat("x", i*5000))}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 5 {
		if got := receive(t, sub); !strings.HasPrefix(got, fmt.Sprintf("%d:", i)) || len(got) != 2+i*5000 {
			t.Fatalf("message %d out of order or damaged: %q...", i, got[:min(len(got), 8)])
		}
	}
}

func TestMessagesOverTheLimitAreRefused(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	broker := newBroker(t, db, bcpostgres.Options{MaxMessageBytes: 10000})
	ctx := context.Background()
	channel := global(t, "posts")
	sub, _ := broker.Subscribe(ctx, channel)
	if err := broker.Publish(ctx, channel, broadcast.Message{Data: strings.Repeat("x", 10001)}); !errors.Is(err, broadcast.ErrMessageTooLarge) {
		t.Fatalf("Publish: %v", err)
	}
	expectNothing(t, sub)
	if err := broker.Publish(ctx, channel, broadcast.Message{Data: strings.Repeat("x", 10000)}); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, sub); len(got) != 10000 {
		t.Fatalf("got %d bytes", len(got))
	}
}

func TestPublishTxIsAtomicWithTheTransaction(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	a, b := newBroker(t, db, bcpostgres.Options{}), newBroker(t, db, bcpostgres.Options{})
	ctx := context.Background()
	channel := global(t, "posts")
	onA, _ := a.Subscribe(ctx, channel)
	onB, _ := b.Subscribe(ctx, channel)

	rollback := errors.New("rollback")
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		if err := a.PublishTx(ctx, tx, channel, broadcast.Message{Data: "never"}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	expectNothing(t, onA)
	expectNothing(t, onB)

	if err := db.InTx(ctx, func(tx pgx.Tx) error {
		return a.PublishTx(ctx, tx, channel, broadcast.Message{Data: "committed"})
	}); err != nil {
		t.Fatal(err)
	}
	// Every replica, including the publisher's own, hears it once.
	if got := receive(t, onA); got != "committed" {
		t.Fatalf("A got %q", got)
	}
	if got := receive(t, onB); got != "committed" {
		t.Fatalf("B got %q", got)
	}
	expectNothing(t, onA)
}

func TestMalformedNotificationsAreIgnored(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	broker := newBroker(t, db, bcpostgres.Options{})
	ctx := context.Background()
	channel := global(t, "posts")
	sub, _ := broker.Subscribe(ctx, channel)

	for _, payload := range []string{
		"", "garbage", "v1", "v1 o i 0 1 global/posts !!!", "v1 o i 0 1 nope eA==", "v1 o i 0 1 global/BAD eA==",
		"v1 o i 1 3 global/posts eA==", // orphan continuation
		"v2 o i 0 1 global/posts eA==",
	} {
		if _, err := db.Exec(ctx, "SELECT pg_notify($1, $2)", bcpostgres.DefaultNotifyChannel, payload); err != nil {
			t.Fatal(err)
		}
	}
	expectNothing(t, sub)
	if !broker.Listening() {
		t.Fatal("a malformed notification stopped the listener")
	}
	// A well-formed message from "another replica" still arrives.
	if _, err := db.Exec(ctx, "SELECT pg_notify($1, $2)", bcpostgres.DefaultNotifyChannel, "v1 other i 0 1 global/posts aGVsbG8="); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, sub); got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestListenerReconnectsAfterTheConnectionIsKilled(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	a, b := newBroker(t, db, bcpostgres.Options{}), newBroker(t, db, bcpostgres.Options{})
	ctx := context.Background()
	channel := global(t, "posts")
	sub, _ := a.Subscribe(ctx, channel)

	// Terminate A's LISTEN session (the only one with this query).
	if _, err := db.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE 'LISTEN %' AND pid <> pg_backend_pid() AND datname = current_database()`); err != nil {
		t.Fatal(err)
	}
	// Both listeners were terminated. Wait for them to come back (backoff 1s).
	listeners := func() int {
		var count int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'LISTEN %' AND datname = current_database()`).Scan(&count)
		return count
	}
	if !waittest.Poll(20*time.Second, func() bool { return a.Listening() && b.Listening() && listeners() >= 2 }) {
		t.Fatal("listeners did not reconnect")
	}
	// Notifications sent while a listener is still reconnecting are lost by
	// design, so publish until one arrives.
	giveUp := time.Now().Add(15 * time.Second)
	for {
		if err := b.Publish(ctx, channel, broadcast.Message{Data: "after reconnect"}); err != nil {
			t.Fatal(err)
		}
		select {
		case message := <-sub.Messages():
			if message.Data != "after reconnect" {
				t.Fatalf("got %q", message.Data)
			}
			return
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(giveUp) {
			t.Fatal("no message arrived after the reconnect")
		}
	}
}

func TestShutdownStopsTheListenerAndEndsSubscriptions(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	local, _ := broadcast.NewMemory(context.Background(), broadcast.MemoryOptions{})
	broker, err := bcpostgres.New(context.Background(), db, local, bcpostgres.Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	waitListening(t, broker)
	sub, _ := broker.Subscribe(context.Background(), global(t, "posts"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := broker.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := broker.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if _, ok := <-sub.Messages(); ok || !errors.Is(sub.Err(), broadcast.ErrClosed) {
		t.Fatalf("subscription: err=%v", sub.Err())
	}
	if broker.Listening() {
		t.Fatal("still listening after Shutdown")
	}
	// The dedicated connection went back to the server, not to the pool.
	// The server notices the closed socket asynchronously.
	var listeners int
	if !waittest.Poll(10*time.Second, func() bool {
		if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'LISTEN %' AND datname = current_database()`).Scan(&listeners); err != nil {
			t.Fatal(err)
		}
		return listeners == 0
	}) {
		t.Fatalf("listening sessions = %d", listeners)
	}
}

func TestSeparateNotifyChannelsDoNotMix(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	a := newBroker(t, db, bcpostgres.Options{NotifyChannel: "app_one"})
	b := newBroker(t, db, bcpostgres.Options{NotifyChannel: "app_two"})
	ctx := context.Background()
	channel := global(t, "posts")
	sub, _ := a.Subscribe(ctx, channel)
	if err := b.Publish(ctx, channel, broadcast.Message{Data: "other app"}); err != nil {
		t.Fatal(err)
	}
	expectNothing(t, sub)
}

func TestNewValidatesItsArguments(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	local, _ := broadcast.NewMemory(context.Background(), broadcast.MemoryOptions{})
	defer local.Close()
	for name, opts := range map[string]bcpostgres.Options{
		"upper case":  {NotifyChannel: "Forge"},
		"hyphen":      {NotifyChannel: "a-b"},
		"quote":       {NotifyChannel: `a"b`},
		"too long":    {NotifyChannel: strings.Repeat("a", 64)},
		"negative":    {MaxMessageBytes: -1},
		"leading dig": {NotifyChannel: "1abc"},
	} {
		if _, err := bcpostgres.New(context.Background(), db, local, opts); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := bcpostgres.New(context.Background(), nil, local, bcpostgres.Options{}); err == nil {
		t.Error("nil database accepted")
	}
	if _, err := bcpostgres.New(context.Background(), db, nil, bcpostgres.Options{}); err == nil {
		t.Error("nil local broker accepted")
	}
}
