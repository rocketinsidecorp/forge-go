package cache_test

import (
	"testing"

	"github.com/rocketinsidecorp/forge-go/cache"
	"github.com/rocketinsidecorp/forge-go/cache/cachetest"
)

func TestMemoryConformance(t *testing.T) {
	cachetest.Run(t, func(t testing.TB, o cache.Options) cache.Cache {
		m, err := cache.NewMemory(cache.MemoryOptions{Options: o})
		if err != nil {
			t.Fatal(err)
		}
		return m
	})
}
