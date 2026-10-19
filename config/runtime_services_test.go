package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

func validKey() string { return base64.RawURLEncoding.EncodeToString(make([]byte, 32)) }

func TestRuntimeServicesDefaults(t *testing.T) {
	cfg, err := LoadWithLookup(lookupFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimit.MaxKeys != 100_000 || cfg.Cache.MaxEntries != 10_000 || cfg.Cache.MaxBytes != 64<<20 ||
		cfg.Cache.MaxValueBytes != 1<<20 || cfg.Cache.DefaultTTL != time.Hour ||
		cfg.Storage.MaxUploadBytes != 512<<10 || cfg.Storage.LinkTTL != 15*time.Minute || cfg.Storage.Root != "" {
		t.Fatalf("defaults = %+v %+v %+v", cfg.RateLimit, cfg.Cache, cfg.Storage)
	}
}

func TestRuntimeServicesFromEnvironment(t *testing.T) {
	cfg, err := LoadWithLookup(lookupFrom(map[string]string{
		"FORGE_RATE_LIMIT_MAX_KEYS":      "500",
		"FORGE_CACHE_MAX_ENTRIES":        "42",
		"FORGE_CACHE_MAX_BYTES":          "1048576",
		"FORGE_CACHE_MAX_VALUE_BYTES":    "65536",
		"FORGE_CACHE_DEFAULT_TTL":        "5m",
		"FORGE_STORAGE_ROOT":             "/var/lib/app/storage",
		"FORGE_STORAGE_SIGNING_KEY":      validKey(),
		"FORGE_STORAGE_MAX_UPLOAD_BYTES": "1000000",
		"FORGE_STORAGE_LINK_TTL":         "1h",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimit.MaxKeys != 500 || cfg.Cache.MaxEntries != 42 || cfg.Cache.MaxBytes != 1<<20 || cfg.Cache.MaxValueBytes != 65536 ||
		cfg.Cache.DefaultTTL != 5*time.Minute || cfg.Storage.Root != "/var/lib/app/storage" ||
		cfg.Storage.MaxUploadBytes != 1_000_000 || cfg.Storage.LinkTTL != time.Hour {
		t.Fatalf("config = %+v %+v %+v", cfg.RateLimit, cfg.Cache, cfg.Storage)
	}
	key, err := cfg.Storage.SigningKeyBytes()
	if err != nil || len(key) != 32 {
		t.Fatalf("SigningKeyBytes = %d bytes, %v", len(key), err)
	}
}

func TestStorageSigningKeyIsRedacted(t *testing.T) {
	secret := validKey()
	cfg, err := LoadWithLookup(lookupFrom(map[string]string{"FORGE_STORAGE_SIGNING_KEY": secret}))
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{cfg.Storage.SigningKey.String(), cfg.Storage.SigningKey.GoString()} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("signing key printed: %s", rendered)
		}
	}
}

func TestProductionRequiresAStorageSigningKeyWhenStorageIsUsed(t *testing.T) {
	prod := map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy", "FORGE_HTTP_MAX_BODY_BYTES": "2000000"}
	with := func(extra map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range prod {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	if _, err := LoadWithLookup(lookupFrom(with(nil))); err != nil {
		t.Fatalf("production without storage must not need a signing key: %v", err)
	}
	if _, err := LoadWithLookup(lookupFrom(with(map[string]string{"FORGE_STORAGE_ROOT": "/data"}))); err == nil ||
		!strings.Contains(err.Error(), "FORGE_STORAGE_SIGNING_KEY") {
		t.Fatalf("production storage without a key = %v, want a signing key error", err)
	}
	if _, err := LoadWithLookup(lookupFrom(with(map[string]string{"FORGE_STORAGE_ROOT": "relative/dir", "FORGE_STORAGE_SIGNING_KEY": validKey()}))); err == nil {
		t.Fatal("production accepted a relative storage root")
	}
	if _, err := LoadWithLookup(lookupFrom(with(map[string]string{"FORGE_STORAGE_ROOT": "/data", "FORGE_STORAGE_SIGNING_KEY": validKey()}))); err != nil {
		t.Fatalf("valid production storage: %v", err)
	}
	// Development may omit the key; signing then fails closed in storage.NewSignerFromConfig.
	if _, err := LoadWithLookup(lookupFrom(map[string]string{"FORGE_STORAGE_ROOT": "tmp/storage"})); err != nil {
		t.Fatalf("development storage without a key: %v", err)
	}
}

func TestRuntimeServicesValidationNeverEchoesValues(t *testing.T) {
	bad := map[string]string{
		"FORGE_RATE_LIMIT_MAX_KEYS":      "0",
		"FORGE_CACHE_MAX_ENTRIES":        "-3",
		"FORGE_CACHE_MAX_BYTES":          "lots",
		"FORGE_CACHE_MAX_VALUE_BYTES":    "99999999999999999999",
		"FORGE_CACHE_DEFAULT_TTL":        "soon",
		"FORGE_STORAGE_MAX_UPLOAD_BYTES": "0",
		"FORGE_STORAGE_LINK_TTL":         "tomorrow",
	}
	for key, value := range bad {
		_, err := LoadWithLookup(lookupFrom(map[string]string{key: value}))
		if err == nil {
			t.Errorf("%s=%s accepted", key, value)
			continue
		}
		if !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), value) {
			t.Errorf("%s: error %q must name the variable and not echo the value", key, err)
		}
	}
}

func TestRuntimeServicesRangeChecks(t *testing.T) {
	cases := map[string]map[string]string{
		"value above 8 MiB":          {"FORGE_CACHE_MAX_VALUE_BYTES": "8388609"},
		"byte bound below the value": {"FORGE_CACHE_MAX_BYTES": "1000", "FORGE_CACHE_MAX_VALUE_BYTES": "2000"},
		"ttl above a year":           {"FORGE_CACHE_DEFAULT_TTL": "9000h"},
		"sub-millisecond ttl":        {"FORGE_CACHE_DEFAULT_TTL": "1us"},
		"link ttl above 7 days":      {"FORGE_STORAGE_LINK_TTL": "169h"},
		"link ttl under a second":    {"FORGE_STORAGE_LINK_TTL": "500ms"},
		"upload above 1 GiB":         {"FORGE_STORAGE_MAX_UPLOAD_BYTES": "1073741825"},
		"rate limit keys too many":   {"FORGE_RATE_LIMIT_MAX_KEYS": "10000001"},
		"upload above the body cap":  {"FORGE_STORAGE_ROOT": "/data", "FORGE_STORAGE_MAX_UPLOAD_BYTES": "2000000"},
		"storage root with NUL":      {"FORGE_STORAGE_ROOT": "/da\x00ta"},
	}
	for name, env := range cases {
		if _, err := LoadWithLookup(lookupFrom(env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStorageSigningKeyFormat(t *testing.T) {
	for name, key := range map[string]string{
		"too short":  base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
		"padded":     base64.URLEncoding.EncodeToString(make([]byte, 32)),
		"std base64": strings.NewReplacer("-", "+", "_", "/").Replace(base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("\xfb\xff", 16)))),
		"not base64": "!!!!not-base64!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!",
		"too long":   base64.RawURLEncoding.EncodeToString(make([]byte, 2000)),
	} {
		_, err := LoadWithLookup(lookupFrom(map[string]string{"FORGE_STORAGE_SIGNING_KEY": key}))
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), key) {
			t.Errorf("%s: error echoes the key", name)
		}
	}
}

func TestZeroValueRuntimeServicesStillValidate(t *testing.T) {
	cfg := Default()
	cfg.RateLimit, cfg.Cache, cfg.Storage = RateLimit{}, Cache{}, Storage{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero values must mean defaults: %v", err)
	}
	if cfg.RateLimit.EffectiveMaxKeys() != 100_000 || cfg.Storage.EffectiveMaxUploadBytes() != 512<<10 || cfg.Storage.EffectiveLinkTTL() != 15*time.Minute {
		t.Fatal("effective defaults")
	}
}
