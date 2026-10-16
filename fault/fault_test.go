package fault_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/rocketinsidecorp/forge-go/fault"
)

func TestIsMatchesByCodeAcrossCopies(t *testing.T) {
	t.Parallel()
	notFound := fault.New("task_not_found", "task not found", fault.CategoryNotFound, 0)
	wrapped := fmt.Errorf("get task: %w", notFound.WithCause(errors.New("no rows")))
	if !errors.Is(wrapped, notFound) {
		t.Fatal("sentinel not recognized after WithCause and wrapping")
	}
	if errors.Is(wrapped, fault.New("stale_version", "stale", fault.CategoryConflict, 0)) {
		t.Fatal("different codes must not match")
	}
	if got := fault.From(wrapped).Status(); got != http.StatusNotFound {
		t.Fatalf("status=%d", got)
	}
	if got := fault.New("rate_limited", "slow down", fault.CategoryRateLimited, 0).Status(); got != http.StatusTooManyRequests {
		t.Fatalf("rate limited status=%d", got)
	}
}
