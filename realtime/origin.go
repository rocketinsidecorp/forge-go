package realtime

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/fabriciobonjorno/forge-go/fault"
)

// ErrOriginDenied is returned when a browser request comes from an origin the
// policy does not allow.
var ErrOriginDenied = fault.New("realtime_origin_denied", "origin not allowed", fault.CategoryForbidden, http.StatusForbidden)

// OriginPolicy decides which browser origins may open a realtime connection.
// With cookie authentication, a WebSocket handshake from any site would carry
// the user's cookie, so the Origin header must be checked
// (cross-site WebSocket hijacking). The zero policy is the safe default:
// same-origin only.
type OriginPolicy struct {
	// Allowed lists additional origins as scheme://host[:port], for example
	// "https://app.example.com". Matching is exact apart from case. Use
	// "null" only if you knowingly serve sandboxed documents.
	Allowed []string
	// RequireOrigin rejects requests that carry no Origin header. Browsers
	// always send one on a WebSocket handshake; without it the client is not a
	// browser (a CLI, a server, a test) and cannot be a cross-site attacker,
	// so the default accepts it. Set this if every client is a browser.
	RequireOrigin bool
}

// normalized validates Allowed and returns it in comparison form.
func (p OriginPolicy) normalized() (map[string]struct{}, error) {
	allowed := make(map[string]struct{}, len(p.Allowed))
	for _, raw := range p.Allowed {
		if raw == "null" {
			allowed[raw] = struct{}{}
			continue
		}
		origin, err := url.Parse(raw)
		if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" ||
			origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil {
			return nil, fmt.Errorf("realtime: allowed origin %q must be scheme://host[:port]", raw)
		}
		allowed[strings.ToLower(origin.Scheme+"://"+origin.Host)] = struct{}{}
	}
	return allowed, nil
}

// OriginChecker is a validated OriginPolicy.
type OriginChecker struct {
	allowed map[string]struct{}
	require bool
}

// NewOriginChecker validates policy.
func NewOriginChecker(policy OriginPolicy) (*OriginChecker, error) {
	allowed, err := policy.normalized()
	if err != nil {
		return nil, err
	}
	return &OriginChecker{allowed: allowed, require: policy.RequireOrigin}, nil
}

// Check returns ErrOriginDenied unless the request's Origin is the request's
// own host, an allowed origin, or absent (and not required).
func (c *OriginChecker) Check(r *http.Request) error {
	values := r.Header.Values("Origin")
	switch len(values) {
	case 0:
		if c.require {
			return ErrOriginDenied
		}
		return nil
	case 1:
	default:
		return ErrOriginDenied
	}
	raw := values[0]
	if _, ok := c.allowed[strings.ToLower(raw)]; ok {
		return nil
	}
	origin, err := url.Parse(raw)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" ||
		origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil {
		return ErrOriginDenied
	}
	if !strings.EqualFold(origin.Host, r.Host) {
		return ErrOriginDenied
	}
	// Over TLS the page that opened the connection must have been https too.
	if r.TLS != nil && origin.Scheme != "https" {
		return ErrOriginDenied
	}
	return nil
}
