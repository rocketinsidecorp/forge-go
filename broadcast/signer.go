package broadcast

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

const (
	// MinKeyBytes is the shortest accepted signing key.
	MinKeyBytes = 32
	// DefaultTTL is how long a signed channel name stays valid.
	DefaultTTL = 24 * time.Hour
	// MaxTTL is the longest allowed validity.
	MaxTTL = 7 * 24 * time.Hour

	tokenVersion  = "v1"
	maxTokenBytes = 512
	macLabel      = "forge-broadcast-stream-v1."
)

// Errors returned by Signer.Verify. Handlers answer both the same way.
var (
	ErrInvalidToken = errors.New("broadcast: invalid stream token")
	ErrExpiredToken = errors.New("broadcast: stream token expired")
)

// SignerOptions configures NewSigner.
type SignerOptions struct {
	// Key signs and verifies tokens: at least MinKeyBytes of random data,
	// shared by every replica of the application.
	Key []byte
	// PreviousKeys are still accepted for verification (never for signing),
	// so a key can be rotated without breaking open pages.
	PreviousKeys [][]byte
	// TTL is how long a token is valid. Zero means DefaultTTL; it may not
	// exceed MaxTTL.
	TTL time.Duration
	// Now replaces time.Now, for tests.
	Now func() time.Time
}

// Signer signs channel names with HMAC-SHA256 so that a browser can only
// subscribe to channels the server rendered for it, like Rails' signed stream
// names. A token is a bearer capability for one channel until it expires: it
// does not prove who holds it, so tenant-scoped channels are additionally
// checked against the request's tenant by the Handler.
type Signer struct {
	key  []byte
	prev [][]byte
	ttl  time.Duration
	now  func() time.Time
}

// NewSigner validates opts.
func NewSigner(opts SignerOptions) (*Signer, error) {
	if len(opts.Key) < MinKeyBytes {
		return nil, fmt.Errorf("broadcast: the signing key must be at least %d bytes", MinKeyBytes)
	}
	signer := &Signer{key: append([]byte(nil), opts.Key...), ttl: opts.TTL, now: opts.Now}
	for _, previous := range opts.PreviousKeys {
		if len(previous) < MinKeyBytes {
			return nil, fmt.Errorf("broadcast: previous signing keys must be at least %d bytes", MinKeyBytes)
		}
		signer.prev = append(signer.prev, append([]byte(nil), previous...))
	}
	if signer.ttl == 0 {
		signer.ttl = DefaultTTL
	}
	if signer.ttl < 0 || signer.ttl > MaxTTL {
		return nil, fmt.Errorf("broadcast: the token TTL must be positive and at most %s", MaxTTL)
	}
	if signer.now == nil {
		signer.now = time.Now
	}
	return signer, nil
}

// NewSignerFromBase64 is NewSigner with a base64 (standard or URL-safe,
// padded or not) key such as `openssl rand -base64 32` prints.
func NewSignerFromBase64(key string, opts SignerOptions) (*Signer, error) {
	decoded, err := DecodeKey(key)
	if err != nil {
		return nil, err
	}
	opts.Key = decoded
	return NewSigner(opts)
}

// RandomKey returns MinKeyBytes of random data for a signer that only needs
// to live as long as the process (development and tests). Production keys
// come from configuration so every replica shares them.
func RandomKey() ([]byte, error) {
	key := make([]byte, MinKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("broadcast: generate a key: %w", err)
	}
	return key, nil
}

// DecodeKey decodes a base64 signing key. The error never contains the key.
func DecodeKey(key string) ([]byte, error) {
	key = strings.TrimSpace(key)
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(key); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("broadcast: the signing key is not valid base64")
}

// MaxPreviousKeys is how many previous keys DecodePreviousKeys accepts: a
// rotation needs the old key for one token lifetime, so more than a few means
// retired keys were left in place.
const MaxPreviousKeys = 3

// DecodePreviousKeys decodes a comma-separated list of base64 keys, the
// format of a generated application's FORGE_STREAMS_PREVIOUS_KEYS, for
// SignerOptions.PreviousKeys. An empty list is no keys. At most
// MaxPreviousKeys are accepted and an empty entry is refused. Errors name the
// entry's position, never its content.
func DecodePreviousKeys(list string) ([][]byte, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	entries := strings.Split(list, ",")
	if len(entries) > MaxPreviousKeys {
		return nil, fmt.Errorf("broadcast: at most %d previous signing keys are accepted, got %d", MaxPreviousKeys, len(entries))
	}
	keys := make([][]byte, 0, len(entries))
	for i, entry := range entries {
		if strings.TrimSpace(entry) == "" {
			return nil, fmt.Errorf("broadcast: previous signing key %d is empty", i+1)
		}
		key, err := DecodeKey(entry)
		if err != nil {
			return nil, fmt.Errorf("broadcast: previous signing key %d is not valid base64", i+1)
		}
		if len(key) < MinKeyBytes {
			return nil, fmt.Errorf("broadcast: previous signing key %d must be at least %d bytes", i+1, MinKeyBytes)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

type claims struct {
	Tenant  string `json:"t,omitempty"`
	Name    string `json:"n"`
	Expires int64  `json:"e"`
}

// Sign returns a token for channel that expires after the signer's TTL.
func (s *Signer) Sign(channel Channel) (string, error) {
	if err := channel.valid(); err != nil {
		return "", err
	}
	payload := claims{Name: channel.Name, Expires: s.now().Add(s.ttl).Unix()}
	if !channel.IsGlobal() {
		payload.Tenant = channel.Tenant.String()
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("broadcast: encode token: %w", err)
	}
	body := tokenVersion + "." + base64.RawURLEncoding.EncodeToString(encoded)
	return body + "." + base64.RawURLEncoding.EncodeToString(mac(s.key, body)), nil
}

// Verify checks the signature and expiry of token and returns its channel.
func (s *Signer) Verify(token string) (Channel, error) {
	if len(token) > maxTokenBytes {
		return Channel{}, ErrInvalidToken
	}
	body, signature, ok := cutLast(token, ".")
	if !ok || !strings.HasPrefix(body, tokenVersion+".") {
		return Channel{}, ErrInvalidToken
	}
	given, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil {
		return Channel{}, ErrInvalidToken
	}
	valid := hmac.Equal(given, mac(s.key, body))
	for _, previous := range s.prev {
		valid = hmac.Equal(given, mac(previous, body)) || valid
	}
	if !valid {
		return Channel{}, ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(body, tokenVersion+"."))
	if err != nil {
		return Channel{}, ErrInvalidToken
	}
	var payload claims
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Channel{}, ErrInvalidToken
	}
	if s.now().Unix() >= payload.Expires {
		return Channel{}, ErrExpiredToken
	}
	if payload.Tenant == "" {
		channel, err := Global(payload.Name)
		if err != nil {
			return Channel{}, ErrInvalidToken
		}
		return channel, nil
	}
	tenant, err := uuid.Parse(payload.Tenant)
	if err != nil {
		return Channel{}, ErrInvalidToken
	}
	channel, err := ForTenant(tenant, payload.Name)
	if err != nil {
		return Channel{}, ErrInvalidToken
	}
	return channel, nil
}

// SourceURL returns the URL a <turbo-stream-source> connects to: path (where
// the Handler is mounted, such as "/streams") with the signed channel in the
// "stream" query parameter.
func (s *Signer) SourceURL(path string, channel Channel) (string, error) {
	token, err := s.Sign(channel)
	if err != nil {
		return "", err
	}
	return path + "?" + url.Values{StreamParameter: {token}}.Encode(), nil
}

func mac(key []byte, body string) []byte {
	hash := hmac.New(sha256.New, key)
	hash.Write([]byte(macLabel))
	hash.Write([]byte(body))
	return hash.Sum(nil)
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+len(sep):], true
}
