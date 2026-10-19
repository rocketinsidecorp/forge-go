package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
)

// MinKeyBytes is the shortest signing key NewSigner accepts: a SHA-256
// HMAC key should be as long as the digest.
const MinKeyBytes = 32

// MaxLinkTTL is the longest lifetime a Signer issues.
const MaxLinkTTL = 7 * 24 * time.Hour

// DefaultLinkTTL is the lifetime of a link signed without a TTL.
const DefaultLinkTTL = 15 * time.Minute

const macContext = "forge.storage.url.v1\n"

// Signer issues and verifies time-limited links to stored blobs. A link is a
// capability: whoever holds it can read that one key until it expires, so keep
// lifetimes short and serve links over TLS. Bind a link to a principal
// (SignOptions.Binding) when it must not be forwarded.
//
// The signature is HMAC-SHA256 (crypto/hmac) over the key, the expiry, the
// disposition and the binding, compared in constant time. Verification never
// reveals which part was wrong.
type Signer struct {
	keys [][]byte // keys[0] signs; all verify, so a key can be rotated
	now  func() time.Time
}

// NewSigner returns a Signer that signs with key and also accepts links signed
// with any of previous (key rotation). Every key must be at least MinKeyBytes
// long and come from a secure random source.
func NewSigner(key []byte, previous ...[]byte) (*Signer, error) {
	s := &Signer{now: time.Now}
	for _, k := range append([][]byte{key}, previous...) {
		if len(k) < MinKeyBytes {
			return nil, errors.New("storage signing key must be at least 32 bytes")
		}
		s.keys = append(s.keys, append([]byte(nil), k...))
	}
	return s, nil
}

// NewSignerFromConfig returns a Signer for FORGE_STORAGE_SIGNING_KEY. It fails
// when the key is not configured: Forge never signs with a default key.
func NewSignerFromConfig(cfg config.Storage) (*Signer, error) {
	key, err := cfg.SigningKeyBytes()
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, errors.New("FORGE_STORAGE_SIGNING_KEY is required to sign storage links")
	}
	return NewSigner(key)
}

// SignOptions configures one link.
type SignOptions struct {
	// TTL is the lifetime, at most MaxLinkTTL. Zero means DefaultLinkTTL.
	TTL time.Duration
	// Inline asks to display the file in the browser. It only takes effect for
	// content types that are safe to display (see InlineSafe); everything else
	// is always downloaded.
	Inline bool
	// Binding ties the link to a value the request must also present, such as
	// the session or user ID: Handler recomputes it with HandlerOptions.Binding
	// and a link forwarded to someone else stops working.
	Binding string
}

// Link is a signed capability for one key.
type Link struct {
	Expires   time.Time
	Signature string
	Inline    bool
}

// Query returns the query parameters of the link.
func (l Link) Query() url.Values {
	q := url.Values{
		"expires": {strconv.FormatInt(l.Expires.Unix(), 10)},
		"sig":     {l.Signature},
	}
	if l.Inline {
		q.Set("disposition", "inline")
	}
	return q
}

// Sign signs a link to key.
func (s *Signer) Sign(key string, opts SignOptions) (Link, error) {
	if err := ValidateKey(key); err != nil {
		return Link{}, err
	}
	ttl := opts.TTL
	if ttl == 0 {
		ttl = DefaultLinkTTL
	}
	if ttl < time.Second || ttl > MaxLinkTTL {
		return Link{}, errors.New("storage link ttl must be between 1s and 7 days")
	}
	// Round up to a whole second: a link never lives shorter than its TTL.
	expires := s.now().Add(ttl).Truncate(time.Second).Add(time.Second)
	mac := s.mac(s.keys[0], key, expires.Unix(), opts.Inline, opts.Binding)
	return Link{Expires: expires, Signature: base64.RawURLEncoding.EncodeToString(mac), Inline: opts.Inline}, nil
}

// URL returns a signed URL for key below prefix: a local path such as
// "/files" (the route that mounts Handler, ending before the key) or an
// absolute http(s) URL such as "https://app.example.com/files".
func (s *Signer) URL(prefix, key string, opts SignOptions) (string, error) {
	local := strings.HasPrefix(prefix, "/") && !strings.HasPrefix(prefix, "//")
	if !(local || strings.HasPrefix(prefix, "https://") || strings.HasPrefix(prefix, "http://")) ||
		strings.ContainsAny(prefix, "?#\\ ") {
		return "", errors.New("storage link prefix must be a local path or an http(s) URL without query")
	}
	link, err := s.Sign(key, opts)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(prefix, "/") + "/" + key + "?" + link.Query().Encode(), nil
}

// Verify checks the link parameters in query for key and binding. It returns
// ErrInvalidLink for a missing, malformed or forged parameter, a different
// key, or a different binding, and ErrExpiredLink only for an authentic link
// whose time ran out. inline reports the signed disposition request.
func (s *Signer) Verify(key string, query url.Values, binding string) (inline bool, err error) {
	if ValidateKey(key) != nil {
		return false, ErrInvalidLink
	}
	expires, sig, inline, ok := parseLink(query)
	if !ok {
		return false, ErrInvalidLink
	}
	valid := false
	for _, k := range s.keys {
		if hmac.Equal(sig, s.mac(k, key, expires, inline, binding)) {
			valid = true
		}
	}
	if !valid {
		return false, ErrInvalidLink
	}
	if !s.now().Before(time.Unix(expires, 0)) {
		return false, ErrExpiredLink
	}
	return inline, nil
}

func parseLink(query url.Values) (expires int64, sig []byte, inline, ok bool) {
	e, s, d := query["expires"], query["sig"], query["disposition"]
	if len(e) != 1 || len(s) != 1 || len(d) > 1 || len(e[0]) == 0 || len(e[0]) > 12 {
		return 0, nil, false, false
	}
	for _, c := range []byte(e[0]) {
		if c < '0' || c > '9' {
			return 0, nil, false, false
		}
	}
	expires, err := strconv.ParseInt(e[0], 10, 64)
	if err != nil {
		return 0, nil, false, false
	}
	if len(s[0]) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return 0, nil, false, false
	}
	sig, err = base64.RawURLEncoding.Strict().DecodeString(s[0])
	if err != nil {
		return 0, nil, false, false
	}
	if len(d) == 1 {
		switch d[0] {
		case "inline":
			inline = true
		case "attachment":
		default:
			return 0, nil, false, false
		}
	}
	return expires, sig, inline, true
}

func (s *Signer) mac(key []byte, blobKey string, expires int64, inline bool, binding string) []byte {
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}
	h := hmac.New(sha256.New, key)
	// Every field before the binding is free of newlines (a validated key, a
	// decimal, a fixed word), so the encoding is unambiguous.
	h.Write([]byte(macContext))
	h.Write([]byte(blobKey))
	h.Write([]byte{'\n'})
	h.Write([]byte(strconv.FormatInt(expires, 10)))
	h.Write([]byte{'\n'})
	h.Write([]byte(disposition))
	h.Write([]byte{'\n'})
	h.Write([]byte(binding))
	return h.Sum(nil)
}
