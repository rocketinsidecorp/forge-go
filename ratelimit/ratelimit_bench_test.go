package ratelimit

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// BenchmarkRateLimitDecision is scenario "rate_limit_decision" in
// docs/BENCHMARKS.md: one Memory.Allow call (the in-process limiter, so no
// database round trip).
//
//	allowed       one key under its limit
//	denied        one key over its limit
//	distinct_keys a new key per call in a table that evicts the soonest
//	              expiring bucket when it is full (the worst case: every call
//	              inserts and evicts)
func BenchmarkRateLimitDecision(b *testing.B) {
	ctx := context.Background()
	key := Key("bench", "203.0.113.7")
	b.Run("allowed", func(b *testing.B) {
		m, err := NewMemory(MemoryOptions{})
		if err != nil {
			b.Fatal(err)
		}
		limit := Limit{N: MaxN, Per: time.Hour}
		b.ReportAllocs()
		for b.Loop() {
			if d, err := m.Allow(ctx, key, limit); err != nil || !d.Allowed {
				b.Fatalf("Allow = %+v, %v", d, err)
			}
		}
	})
	b.Run("denied", func(b *testing.B) {
		m, err := NewMemory(MemoryOptions{})
		if err != nil {
			b.Fatal(err)
		}
		limit := Limit{N: 1, Per: time.Hour}
		_, _ = m.Allow(ctx, key, limit)
		b.ReportAllocs()
		for b.Loop() {
			if d, err := m.Allow(ctx, key, limit); err != nil || d.Allowed {
				b.Fatalf("Allow = %+v, %v", d, err)
			}
		}
	})
	b.Run("distinct_keys", func(b *testing.B) {
		m, err := NewMemory(MemoryOptions{MaxKeys: 1000})
		if err != nil {
			b.Fatal(err)
		}
		keys := make([]string, 4096)
		for i := range keys {
			keys[i] = Key("bench", strconv.Itoa(i))
		}
		limit := Limit{N: 10, Per: time.Hour}
		i := 0
		b.ReportAllocs()
		for b.Loop() {
			if _, err := m.Allow(ctx, keys[i%len(keys)], limit); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}
