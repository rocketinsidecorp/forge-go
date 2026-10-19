package broadcast_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/broadcast"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func newMemory(t testing.TB, opts broadcast.MemoryOptions) *broadcast.Memory {
	t.Helper()
	memory, err := broadcast.NewMemory(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(memory.Close)
	return memory
}

func recv(t testing.TB, sub broadcast.Subscription) (broadcast.Message, bool) {
	t.Helper()
	select {
	case message, ok := <-sub.Messages():
		return message, ok
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a message")
		return broadcast.Message{}, false
	}
}

func TestMemoryDeliversInOrderToEverySubscriberOfTheChannelOnly(t *testing.T) {
	t.Parallel()
	memory := newMemory(t, broadcast.MemoryOptions{})
	ctx := context.Background()
	posts, other := mustGlobal(t, "posts"), mustGlobal(t, "comments")
	tenantPosts := mustTenant(t, uuid.MustNew(), "posts")
	first, _ := memory.Subscribe(ctx, posts)
	second, _ := memory.Subscribe(ctx, posts)
	elsewhere, _ := memory.Subscribe(ctx, other)
	scoped, _ := memory.Subscribe(ctx, tenantPosts)
	for i := range 3 {
		if err := memory.Publish(ctx, posts, broadcast.Message{Data: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, sub := range []broadcast.Subscription{first, second} {
		for i := range 3 {
			if message, _ := recv(t, sub); message.Data != fmt.Sprint(i) {
				t.Fatalf("message %d = %q", i, message.Data)
			}
		}
	}
	for name, sub := range map[string]broadcast.Subscription{"other channel": elsewhere, "tenant channel": scoped} {
		select {
		case message := <-sub.Messages():
			t.Fatalf("%s received %q", name, message.Data)
		default:
		}
	}
}

func TestMemoryDropsSlowConsumersWithoutBlockingThePublisher(t *testing.T) {
	t.Parallel()
	memory := newMemory(t, broadcast.MemoryOptions{Buffer: 2})
	ctx := context.Background()
	channel := mustGlobal(t, "posts")
	slow, _ := memory.Subscribe(ctx, channel)
	fast, _ := memory.Subscribe(ctx, channel)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 5 {
			if err := memory.Publish(ctx, channel, broadcast.Message{Data: fmt.Sprint(i)}); err != nil {
				t.Error(err)
			}
			// The fast consumer keeps up.
			if i < 2 {
				recv(t, fast)
			} else {
				select {
				case <-fast.Messages():
				default:
				}
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}

	// The slow subscriber got its buffered messages, then its queue closed.
	var got []string
	for {
		message, ok := <-slow.Messages()
		if !ok {
			break
		}
		got = append(got, message.Data)
	}
	if len(got) != 2 || got[0] != "0" || got[1] != "1" {
		t.Fatalf("slow subscriber received %v, want the 2 buffered messages", got)
	}
	if !errors.Is(slow.Err(), broadcast.ErrSlowConsumer) {
		t.Fatalf("Err = %v, want ErrSlowConsumer", slow.Err())
	}
	if memory.Dropped() != 1 || memory.Subscribers() != 1 {
		t.Fatalf("dropped=%d subscribers=%d", memory.Dropped(), memory.Subscribers())
	}
	// The dropped subscriber's capacity is released and the fast one still works.
	if err := memory.Publish(ctx, channel, broadcast.Message{Data: "after"}); err != nil {
		t.Fatal(err)
	}
	if message, _ := recv(t, fast); message.Data == "" {
		t.Fatal("the healthy subscriber stopped receiving")
	}
}

func TestMemoryBoundsSubscribers(t *testing.T) {
	t.Parallel()
	memory := newMemory(t, broadcast.MemoryOptions{MaxSubscribers: 3, MaxSubscribersPerChannel: 2})
	ctx := context.Background()
	a, b := mustGlobal(t, "a"), mustGlobal(t, "b")
	s1, err := memory.Subscribe(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Subscribe(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Subscribe(ctx, a); !errors.Is(err, broadcast.ErrTooManySubscribers) {
		t.Fatalf("per-channel limit: %v", err)
	}
	if _, err := memory.Subscribe(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Subscribe(ctx, b); !errors.Is(err, broadcast.ErrTooManySubscribers) {
		t.Fatalf("total limit: %v", err)
	}
	// Closing releases capacity, and is idempotent.
	s1.Close()
	s1.Close()
	if memory.Subscribers() != 2 {
		t.Fatalf("subscribers = %d", memory.Subscribers())
	}
	if _, err := memory.Subscribe(ctx, b); err != nil {
		t.Fatalf("capacity was not released: %v", err)
	}
	if _, ok := <-s1.Messages(); ok || s1.Err() != nil {
		t.Fatalf("closed subscription: err=%v", s1.Err())
	}
}

func TestMemoryRejectsOversizedAndInvalidPublishes(t *testing.T) {
	t.Parallel()
	memory := newMemory(t, broadcast.MemoryOptions{MaxMessageBytes: 8})
	ctx := context.Background()
	channel := mustGlobal(t, "a")
	sub, _ := memory.Subscribe(ctx, channel)
	if err := memory.Publish(ctx, channel, broadcast.Message{Data: "123456789"}); !errors.Is(err, broadcast.ErrMessageTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
	if err := memory.Publish(ctx, channel, broadcast.Message{Data: "12345678"}); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if message, _ := recv(t, sub); message.Data != "12345678" {
		t.Fatal("message lost")
	}
	if err := memory.Publish(ctx, broadcast.Channel{}, broadcast.Message{}); err == nil {
		t.Fatal("invalid channel accepted")
	}
	if _, err := memory.Subscribe(ctx, broadcast.Channel{Name: "BAD"}); err == nil {
		t.Fatal("invalid channel accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := memory.Publish(cancelled, channel, broadcast.Message{}); err == nil {
		t.Fatal("publish ignored a cancelled context")
	}
	if _, err := memory.Subscribe(cancelled, channel); err == nil {
		t.Fatal("subscribe ignored a cancelled context")
	}
	if _, err := broadcast.NewMemory(nil, broadcast.MemoryOptions{}); err == nil { //nolint:staticcheck // the nil context is the case under test
		t.Fatal("nil context accepted")
	}
	if _, err := broadcast.NewMemory(ctx, broadcast.MemoryOptions{Buffer: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
}

func TestMemoryCloseEndsEverySubscription(t *testing.T) {
	t.Parallel()
	memory := newMemory(t, broadcast.MemoryOptions{})
	ctx := context.Background()
	channel := mustGlobal(t, "a")
	subs := make([]broadcast.Subscription, 3)
	for i := range subs {
		subs[i], _ = memory.Subscribe(ctx, channel)
	}
	if err := memory.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	memory.Close() // idempotent
	for _, sub := range subs {
		if _, ok := recv(t, sub); ok || !errors.Is(sub.Err(), broadcast.ErrClosed) {
			t.Fatalf("subscription not closed with ErrClosed: err=%v", sub.Err())
		}
	}
	if _, err := memory.Subscribe(ctx, channel); !errors.Is(err, broadcast.ErrClosed) {
		t.Fatalf("subscribe after close: %v", err)
	}
	if err := memory.Publish(ctx, channel, broadcast.Message{}); !errors.Is(err, broadcast.ErrClosed) {
		t.Fatalf("publish after close: %v", err)
	}
	if memory.Subscribers() != 0 {
		t.Fatalf("subscribers = %d", memory.Subscribers())
	}
}

func TestMemoryClosesWhenItsContextIsCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	memory, err := broadcast.NewMemory(ctx, broadcast.MemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := memory.Subscribe(context.Background(), mustGlobal(t, "a"))
	cancel()
	if _, ok := recv(t, sub); ok || !errors.Is(sub.Err(), broadcast.ErrClosed) {
		t.Fatalf("err = %v", sub.Err())
	}
}

func TestMemorySubscriptionEndsWithItsContext(t *testing.T) {
	t.Parallel()
	memory := newMemory(t, broadcast.MemoryOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	sub, _ := memory.Subscribe(ctx, mustGlobal(t, "a"))
	cancel()
	if _, ok := recv(t, sub); ok || sub.Err() != nil {
		t.Fatalf("err = %v", sub.Err())
	}
	if memory.Subscribers() != 0 {
		t.Fatal("subscription leaked")
	}
}

func TestMemoryConcurrentUseIsRaceFreeAndLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	memory, err := broadcast.NewMemory(context.Background(), broadcast.MemoryOptions{Buffer: 4, MaxSubscribers: 64})
	if err != nil {
		t.Fatal(err)
	}
	channel := mustGlobal(t, "a")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 50 {
				if sub, err := memory.Subscribe(context.Background(), channel); err == nil {
					select {
					case <-sub.Messages():
					case <-time.After(time.Millisecond):
					}
					sub.Close()
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				_ = memory.Publish(context.Background(), channel, broadcast.Message{Data: "x"})
			}
		}()
	}
	wg.Wait()
	memory.Close()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}
