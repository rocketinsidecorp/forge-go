package health_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/health"
)

func TestRegistryReady(t *testing.T) {
	t.Parallel()
	registry := health.New()
	if err := registry.Register("database", func(context.Context) error { return errors.New("down") }); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("cache", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	result := registry.Ready(context.Background())
	if result.Healthy || len(result.Checks) != 2 || result.Checks[0].Name != "cache" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestReadyBoundsEachCheck(t *testing.T) {
	t.Parallel()
	registry := health.NewWithTimeout(50 * time.Millisecond)
	if err := registry.Register("hung", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result := registry.Ready(context.Background())
	if result.Healthy || time.Since(started) > time.Second {
		t.Fatalf("hung check: healthy=%v after %v", result.Healthy, time.Since(started))
	}
}
