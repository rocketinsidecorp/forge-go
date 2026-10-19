package ratelimit_test

import (
	"testing"

	"github.com/rocketinsidecorp/forge-go/ratelimit"
	"github.com/rocketinsidecorp/forge-go/ratelimit/limitertest"
)

func TestMemoryConformance(t *testing.T) {
	limitertest.Run(t, func(t testing.TB) ratelimit.Limiter {
		m, err := ratelimit.NewMemory(ratelimit.MemoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return m
	})
}
