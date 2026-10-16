package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/queue"
)

// Timestamps are fixed-width text; a five-digit year would sort before 2026
// and make the job due immediately, claimed again after every lease.
func TestRunAtOutsideTheSupportedYearsIsRejected(t *testing.T) {
	_, store, registry, _ := newTxFixture(t)
	for _, year := range []int{10000, 999, 0} {
		_, err := queue.Enqueue(context.Background(), store, registry, "order.confirm", order{ID: "x"}, queue.RunAt(time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)))
		if err == nil {
			t.Errorf("year %d accepted", year)
		}
	}
	if _, ok, err := store.Claim(context.Background(), queue.ClaimRequest{Queue: "default", Kinds: []string{"order.confirm"}, WorkerID: "w", Lease: time.Minute}); ok || err != nil {
		t.Fatalf("a rejected job was stored and claimed: %v %v", ok, err)
	}
	for _, year := range []int{1000, 9999} {
		if _, err := queue.Enqueue(context.Background(), store, registry, "order.confirm", order{ID: "x"}, queue.RunAt(time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC))); err != nil {
			t.Errorf("year %d rejected: %v", year, err)
		}
	}
}
