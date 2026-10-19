package config

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// RateLimit configures the in-memory rate limiter (ADR 0031). Zero values
// select the defaults, so a Config built without Default still validates.
type RateLimit struct {
	// MaxKeys bounds the buckets the in-memory limiter holds
	// (FORGE_RATE_LIMIT_MAX_KEYS, default 100000).
	MaxKeys int
}

// Cache configures the application cache (ADR 0032). Zero values select the
// defaults.
type Cache struct {
	// MaxEntries bounds the entries of a cache (FORGE_CACHE_MAX_ENTRIES,
	// default 10000).
	MaxEntries int
	// MaxBytes bounds the memory of the in-memory cache
	// (FORGE_CACHE_MAX_BYTES, default 64 MiB).
	MaxBytes int64
	// MaxValueBytes bounds one cached value (FORGE_CACHE_MAX_VALUE_BYTES,
	// default 1 MiB, at most 8 MiB).
	MaxValueBytes int
	// DefaultTTL applies when a write names no time to live
	// (FORGE_CACHE_DEFAULT_TTL, default 1h).
	DefaultTTL time.Duration
}

// Storage configures file storage (ADR 0033). An empty Root means the
// application does not use disk storage.
type Storage struct {
	// Root is the directory the disk adapter stores files in
	// (FORGE_STORAGE_ROOT). In production it must be an absolute path on a
	// persistent volume.
	Root string
	// SigningKey is the canonical unpadded base64url encoding of at least 32
	// random bytes (FORGE_STORAGE_SIGNING_KEY), the HMAC key of signed links.
	// Production requires it whenever Root is set. Generate one with
	// "openssl rand -base64 32 | tr '+/' '-_' | tr -d '='".
	SigningKey Secret
	// MaxUploadBytes bounds one uploaded file (FORGE_STORAGE_MAX_UPLOAD_BYTES,
	// default 512 KiB). When Root is set it cannot exceed
	// FORGE_HTTP_MAX_BODY_BYTES, which caps every request body.
	MaxUploadBytes int64
	// LinkTTL is the lifetime of signed links (FORGE_STORAGE_LINK_TTL, default
	// 15m, at most 7 days).
	LinkTTL time.Duration
}

const (
	maxRateLimitKeys    = 10_000_000
	maxCacheEntries     = 100_000_000
	maxCacheValueBytes  = 8 << 20
	maxCacheBytes       = 1 << 40
	maxStorageUpload    = 1 << 30
	maxStorageLinkTTL   = 7 * 24 * time.Hour
	minStorageKeyBytes  = 32
	maxStorageKeyBytes  = 1024
	defaultRateLimitMax = 100_000
	defaultStorageBytes = 512 << 10
	defaultStorageTTL   = 15 * time.Minute
)

// EffectiveMaxKeys returns MaxKeys, or the default when unset.
func (r RateLimit) EffectiveMaxKeys() int {
	if r.MaxKeys == 0 {
		return defaultRateLimitMax
	}
	return r.MaxKeys
}

func (r RateLimit) validate() error {
	if r.MaxKeys < 0 || r.MaxKeys > maxRateLimitKeys {
		return errors.New("FORGE_RATE_LIMIT_MAX_KEYS must be between 1 and 10000000")
	}
	return nil
}

func (c Cache) validate() error {
	switch {
	case c.MaxEntries < 0 || c.MaxEntries > maxCacheEntries:
		return errors.New("FORGE_CACHE_MAX_ENTRIES must be between 1 and 100000000")
	case c.MaxBytes < 0 || c.MaxBytes > maxCacheBytes:
		return errors.New("FORGE_CACHE_MAX_BYTES must be positive and at most 1 TiB")
	case c.MaxValueBytes < 0 || c.MaxValueBytes > maxCacheValueBytes:
		return errors.New("FORGE_CACHE_MAX_VALUE_BYTES must be between 1 and 8388608")
	case c.DefaultTTL < 0 || (c.DefaultTTL != 0 && c.DefaultTTL < time.Millisecond) || c.DefaultTTL > 365*24*time.Hour:
		return errors.New("FORGE_CACHE_DEFAULT_TTL must be between 1ms and 8760h")
	case c.MaxBytes != 0 && c.MaxValueBytes != 0 && c.MaxBytes < int64(c.MaxValueBytes)+4096:
		return errors.New("FORGE_CACHE_MAX_BYTES must exceed FORGE_CACHE_MAX_VALUE_BYTES by at least 4096")
	}
	return nil
}

// SigningKeyBytes decodes SigningKey. It returns nil, nil when no key is
// configured and an error (that never contains the value) when it is not
// canonical unpadded base64url of 32 to 1024 bytes.
func (s Storage) SigningKeyBytes() ([]byte, error) {
	if s.SigningKey.IsZero() {
		return nil, nil
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(s.SigningKey.Reveal())
	if err != nil || len(key) < minStorageKeyBytes || len(key) > maxStorageKeyBytes {
		return nil, errors.New("FORGE_STORAGE_SIGNING_KEY must be unpadded base64url of 32 to 1024 random bytes")
	}
	return key, nil
}

// EffectiveMaxUploadBytes returns MaxUploadBytes, or the default when unset.
func (s Storage) EffectiveMaxUploadBytes() int64 {
	if s.MaxUploadBytes == 0 {
		return defaultStorageBytes
	}
	return s.MaxUploadBytes
}

// EffectiveLinkTTL returns LinkTTL, or the default when unset.
func (s Storage) EffectiveLinkTTL() time.Duration {
	if s.LinkTTL == 0 {
		return defaultStorageTTL
	}
	return s.LinkTTL
}

func (s Storage) validate(environment Environment, maxBodyBytes int64) error {
	if strings.ContainsRune(s.Root, 0) {
		return errors.New("FORGE_STORAGE_ROOT is invalid")
	}
	if _, err := s.SigningKeyBytes(); err != nil {
		return err
	}
	if s.MaxUploadBytes < 0 || s.MaxUploadBytes > maxStorageUpload {
		return errors.New("FORGE_STORAGE_MAX_UPLOAD_BYTES must be between 1 and 1073741824")
	}
	if s.LinkTTL < 0 || (s.LinkTTL != 0 && s.LinkTTL < time.Second) || s.LinkTTL > maxStorageLinkTTL {
		return errors.New("FORGE_STORAGE_LINK_TTL must be between 1s and 168h")
	}
	if s.Root == "" {
		return nil
	}
	if s.EffectiveMaxUploadBytes() > maxBodyBytes {
		return errors.New("FORGE_STORAGE_MAX_UPLOAD_BYTES cannot exceed FORGE_HTTP_MAX_BODY_BYTES, which caps every request body")
	}
	if environment == Production {
		if !strings.HasPrefix(s.Root, "/") {
			return errors.New("production requires FORGE_STORAGE_ROOT to be an absolute path")
		}
		if s.SigningKey.IsZero() {
			return errors.New("production requires FORGE_STORAGE_SIGNING_KEY when FORGE_STORAGE_ROOT is set")
		}
	}
	return nil
}
