package broadcast

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/web"
)

// StreamParameter is the query parameter carrying the signed channel name.
const StreamParameter = "stream"

// Defaults of the event stream handler.
const (
	DefaultHeartbeat        = 20 * time.Second
	DefaultMaxConnectionAge = 30 * time.Minute
	DefaultWriteTimeout     = 10 * time.Second
	// DefaultRetry is the reconnection delay clients are told to use.
	DefaultRetry = 3 * time.Second
)

// Authorizer decides whether the request may receive channel, after the token
// is verified. Return a *fault.Error (such as fault.New("permission_denied",
// ..., fault.CategoryForbidden, 0)) to refuse; any other error is a 500.
type Authorizer func(r *http.Request, channel Channel) error

// HandlerOptions configures NewHandler.
type HandlerOptions struct {
	// Broker supplies the messages. Required.
	Broker Broker
	// Signer verifies channel names. Required.
	Signer *Signer
	// Authorize is an extra, application-specific check, such as a
	// permission. It runs after the built-in checks, which already require a
	// tenant-scoped channel's tenant to equal the request's tenant. Mount the
	// handler behind your authentication middleware so the request carries a
	// principal and tenant.
	Authorize Authorizer
	// Heartbeat is how often a comment line keeps idle connections open
	// through proxies. Zero means DefaultHeartbeat.
	Heartbeat time.Duration
	// MaxConnectionAge ends a stream after this long; the browser reconnects
	// on its own, which re-runs authorization, so a revoked session or a
	// removed permission stops receiving within this time. Zero means
	// DefaultMaxConnectionAge; negative disables the limit.
	MaxConnectionAge time.Duration
	// WriteTimeout bounds each write to a slow client. Zero means
	// DefaultWriteTimeout.
	WriteTimeout time.Duration
	// Logger receives debug and error events. Nil means slog.Default.
	Logger *slog.Logger
}

type handler struct {
	opts HandlerOptions
}

// NewHandler returns the Server-Sent Events endpoint a <turbo-stream-source>
// connects to: GET with the signed channel in the "stream" query parameter.
//
// Checks, in order, all failing closed: method; token signature and expiry
// (403 stream_invalid, expired and forged tokens look alike); for a
// tenant-scoped channel the request must carry the same tenant
// (tenancy.FromContext), otherwise 403 stream_forbidden; the Authorize hook;
// capacity (503 with Retry-After).
//
// Each connection is served by its request goroutine. The handler clears the
// server's per-request read and write deadlines with http.ResponseController
// (a stream outlives http.Server.ReadTimeout and WriteTimeout) and applies
// WriteTimeout to every write instead, so a stalled client cannot hold a
// connection forever. A connection also ends when the client leaves, the
// subscription is dropped (a slow consumer) or closed (broker shutdown), or
// MaxConnectionAge passes.
func NewHandler(opts HandlerOptions) (http.Handler, error) {
	if opts.Broker == nil || opts.Signer == nil {
		return nil, errors.New("broadcast: a broker and a signer are required")
	}
	if opts.Heartbeat < 0 || opts.WriteTimeout < 0 {
		return nil, errors.New("broadcast: heartbeat and write timeout cannot be negative")
	}
	if opts.Heartbeat == 0 {
		opts.Heartbeat = DefaultHeartbeat
	}
	if opts.MaxConnectionAge == 0 {
		opts.MaxConnectionAge = DefaultMaxConnectionAge
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = DefaultWriteTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &handler{opts: opts}, nil
}

var (
	errStreamInvalid   = fault.New("stream_invalid", "stream is invalid or expired", fault.CategoryForbidden, 0)
	errStreamForbidden = fault.New("stream_forbidden", "stream is not available to this account", fault.CategoryForbidden, 0)
	errStreamBusy      = fault.New("stream_unavailable", "too many open streams; try again shortly", fault.CategoryUnavailable, 0)
)

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		web.Error(w, r, fault.New("method_not_allowed", "method not allowed", fault.CategoryInvalid, http.StatusMethodNotAllowed))
		return
	}
	query := r.URL.Query()
	tokens := query[StreamParameter]
	if len(tokens) != 1 {
		web.Error(w, r, errStreamInvalid)
		return
	}
	channel, err := h.opts.Signer.Verify(tokens[0])
	if err != nil {
		h.opts.Logger.DebugContext(r.Context(), "event stream refused", "reason", err.Error())
		web.Error(w, r, errStreamInvalid)
		return
	}
	if !channel.IsGlobal() {
		tenant, ok := tenancy.FromContext(r.Context())
		if !ok || tenant.ID != channel.Tenant {
			web.Error(w, r, errStreamForbidden)
			return
		}
	}
	if h.opts.Authorize != nil {
		if err := h.opts.Authorize(r, channel); err != nil {
			web.Error(w, r, err)
			return
		}
	}
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		web.Error(w, r, fmt.Errorf("broadcast: clear read deadline: %w", err))
		return
	}
	subscription, err := h.opts.Broker.Subscribe(r.Context(), channel)
	switch {
	case errors.Is(err, ErrTooManySubscribers), errors.Is(err, ErrClosed):
		w.Header().Set("Retry-After", "5")
		web.Error(w, r, errStreamBusy)
		return
	case err != nil:
		web.Error(w, r, err)
		return
	}
	defer subscription.Close()
	h.stream(w, r, controller, subscription)
}

func (h *handler) stream(w http.ResponseWriter, r *http.Request, controller *http.ResponseController, subscription Subscription) {
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
	w.WriteHeader(http.StatusOK)

	write := func(text string) bool {
		// A new deadline per write replaces the server's WriteTimeout, which
		// would otherwise end the stream.
		if err := controller.SetWriteDeadline(time.Now().Add(h.opts.WriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if _, err := fmt.Fprint(w, text); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !write(fmt.Sprintf("retry: %d\n: connected\n\n", DefaultRetry.Milliseconds())) {
		return
	}

	heartbeat := time.NewTicker(h.opts.Heartbeat)
	defer heartbeat.Stop()
	var expired <-chan time.Time
	if h.opts.MaxConnectionAge > 0 {
		timer := time.NewTimer(h.opts.MaxConnectionAge)
		defer timer.Stop()
		expired = timer.C
	}
	for {
		select {
		case message, open := <-subscription.Messages():
			if !open {
				if err := subscription.Err(); err != nil && !errors.Is(err, ErrClosed) {
					h.opts.Logger.DebugContext(r.Context(), "event stream ended", "reason", err.Error())
				}
				return
			}
			if !write(event(message.Data)) {
				return
			}
		case <-heartbeat.C:
			if !write(": ping\n\n") {
				return
			}
		case <-expired:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// event formats data as one SSE event. Every line, split on any of the line
// terminators the SSE parser recognizes (\n, \r\n, \r), becomes its own
// "data:" field, so the payload cannot end the event early or add fields.
func event(data string) string {
	var out strings.Builder
	out.Grow(len(data) + 16)
	start := 0
	flush := func(end, next int) {
		out.WriteString("data: ")
		out.WriteString(data[start:end])
		out.WriteByte('\n')
		start = next
	}
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			flush(i, i+1)
		case '\r':
			next := i + 1
			if next < len(data) && data[next] == '\n' {
				next++
			}
			flush(i, next)
			i = next - 1
		}
	}
	flush(len(data), len(data))
	out.WriteByte('\n')
	return out.String()
}
