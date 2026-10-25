package config_test

import (
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
)

func TestLoadReadsTheProcessEnvironment(t *testing.T) {
	// Not parallel: the process environment.
	t.Setenv("FORGE_ENV", "test")
	t.Setenv("FORGE_HTTP_ADDR", "127.0.0.1:18080")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Environment != config.Test || cfg.HTTP.Address != "127.0.0.1:18080" {
		t.Errorf("Load = %+v", cfg.HTTP)
	}
}

func TestLoadWithLookupRefusesMalformedNumbers(t *testing.T) {
	t.Parallel()
	if _, err := config.LoadWithLookup(nil); err == nil {
		t.Error("a nil lookup was accepted")
	}
	base := map[string]string{"FORGE_ENV": "test"}
	for name, bad := range map[string]map[string]string{
		"header bytes":      {"FORGE_HTTP_MAX_HEADER_BYTES": "lots"},
		"body bytes":        {"FORGE_HTTP_MAX_BODY_BYTES": "1MB"},
		"rate limit keys":   {"FORGE_RATE_LIMIT_MAX_KEYS": "0"},
		"cache entries":     {"FORGE_CACHE_MAX_ENTRIES": "-3"},
		"cache value bytes": {"FORGE_CACHE_MAX_VALUE_BYTES": "xyzzy"},
		"cache bytes":       {"FORGE_CACHE_MAX_BYTES": "1.5"},
		"upload bytes":      {"FORGE_STORAGE_MAX_UPLOAD_BYTES": ""},
	} {
		values := map[string]string{}
		for k, v := range base {
			values[k] = v
		}
		for k, v := range bad {
			values[k] = v
		}
		_, err := config.LoadWithLookup(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
		if err == nil {
			t.Errorf("%s: accepted %v", name, bad)
			continue
		}
		for key, value := range bad {
			if !strings.Contains(err.Error(), key) {
				t.Errorf("%s: error %q does not name %s", name, err, key)
			}
			if value != "" && strings.Contains(err.Error(), value) {
				t.Errorf("%s: error %q echoes the value", name, err)
			}
		}
	}
}
