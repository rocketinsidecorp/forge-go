package ratelimit

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/web"
)

// ErrRateLimited is the fault a limited request is answered with (429).
var ErrRateLimited = fault.New("rate_limited", "too many requests", fault.CategoryRateLimited, http.StatusTooManyRequests)

// ErrUnavailable is the fault answered when the limiter fails and the
// middleware fails closed (503).
var ErrUnavailable = fault.New("rate_limit_unavailable", "the service is temporarily unavailable", fault.CategoryUnavailable, http.StatusServiceUnavailable)

// KeyFunc identifies the client of a request: a remote address, a session or
// user ID, an API key ID, or a combination (see Join). The value is hashed
// by Key before it reaches the limiter, so it may be personal data, but it
// must not be chosen by the client: a header or form field the client sets
// freely lets one client use unlimited keys. An empty result puts the
// request in the shared bucket of unidentified clients (it is limited, never
// exempt).
type KeyFunc func(r *http.Request) string

// ByRemoteAddr keys by the peer address of the TCP connection, the only
// address a client cannot forge. IPv4 addresses are used whole; IPv6
// addresses are reduced to their /64 prefix, because one subscriber
// routinely controls a whole /64 and could otherwise rotate through
// addresses.
//
// Behind a reverse proxy (FORGE_HTTP_TRANSPORT=trusted-proxy) the peer is
// the proxy, so every client shares one bucket. Forge does not interpret
// X-Forwarded-For (see the roadmap): key by something the application
// authenticates (the session or user ID), or write a KeyFunc that reads the
// one header your proxy overwrites, only when the proxy is the sole path to
// the application.
func ByRemoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		prefix, err := addr.Prefix(64)
		if err == nil {
			return prefix.String()
		}
	}
	return addr.String()
}

// Join combines several identifying values into one unambiguous KeyFunc
// result (for example a user ID and a remote address).
func Join(parts ...string) string {
	return Key("join", parts...)
}

// Option customizes Middleware.
type Option func(*middleware) error

// WithScope names what the limit protects. Limits with the same scope, key
// and window length share a counter, so give distinct routes distinct scopes
// when they must be counted separately. The default scope is the matched
// route pattern (http.Request.Pattern), so a middleware applied to one route
// counts that route alone, and one applied to the whole application counts
// all routes together.
func WithScope(scope string) Option {
	return func(m *middleware) error {
		if scope == "" {
			return errors.New("rate limit scope must not be empty")
		}
		m.scope = scope
		return nil
	}
}

// FailOpen lets requests through when the limiter returns an error (for
// example the database is down), after logging it. The default is to fail
// closed with 503, because an attacker who can make the limiter fail would
// otherwise be unlimited.
func FailOpen() Option {
	return func(m *middleware) error {
		m.failOpen = true
		return nil
	}
}

type middleware struct {
	limiter  Limiter
	key      KeyFunc
	limit    Limit
	scope    string
	failOpen bool
}

// Middleware returns middleware that allows at most limit.N requests per
// limit.Per for each client identified by key, and answers the rest with 429
// and a Retry-After header: in the JSON error format, or as a minimal HTML
// page when the client prefers HTML. It is a httpserver.Middleware: pass it
// to App.Use for the whole application, or wrap one route, like Rails'
// `rate_limit to: 10, within: 3.minutes, only: :create`:
//
//	create, err := ratelimit.Middleware(limiter, ratelimit.ByRemoteAddr,
//	    ratelimit.Limit{N: 10, Per: 3 * time.Minute})
//	if err != nil { return err }
//	err = r.Handle("POST /comments", create(http.HandlerFunc(h.Create)))
//
// Requests are counted before the handler runs, whatever the handler's
// outcome. Place it after authentication when keying by principal.
func Middleware(limiter Limiter, key KeyFunc, limit Limit, opts ...Option) (func(http.Handler) http.Handler, error) {
	if limiter == nil || key == nil {
		return nil, errors.New("rate limit middleware needs a limiter and a key function")
	}
	if err := limit.Validate(); err != nil {
		return nil, err
	}
	m := &middleware{limiter: limiter, key: key, limit: limit}
	for _, opt := range opts {
		if err := opt(m); err != nil {
			return nil, err
		}
	}
	return func(next http.Handler) http.Handler {
		if next == nil {
			panic("ratelimit: nil handler")
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.serve(w, r, next)
		})
	}, nil
}

func (m *middleware) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	scope := m.scope
	if scope == "" {
		scope = r.Pattern
	}
	storeKey := Key(scope, strconv.FormatInt(int64(m.limit.Per), 10), m.key(r))
	decision, err := m.limiter.Allow(r.Context(), storeKey, m.limit)
	switch {
	case err != nil && m.failOpen:
		web.Logger(r.Context()).Warn("rate limiter unavailable; failing open", "error", err)
	case err != nil:
		web.Error(w, r, ErrUnavailable.WithCause(fmt.Errorf("rate limiter: %w", err)))
		return
	case !decision.Allowed:
		limited(w, r, decision)
		return
	}
	next.ServeHTTP(w, r)
}

const limitedPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Too many requests</title></head>
<body><h1>Too many requests</h1><p>You have made too many requests. Please wait and try again.</p></body></html>
`

func limited(w http.ResponseWriter, r *http.Request, decision Decision) {
	w.Header().Set("Retry-After", strconv.Itoa(RetryAfterSeconds(decision.RetryAfter)))
	w.Header().Set("Cache-Control", "no-store")
	if web.PrefersHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(limitedPage))
		return
	}
	web.Error(w, r, ErrRateLimited)
}
