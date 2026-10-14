package realtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fabriciobonjorno/forge-go/fault"
	"github.com/fabriciobonjorno/forge-go/uuid"
	"github.com/fabriciobonjorno/forge-go/web"
)

// SSE defaults.
const (
	DefaultHeartbeatInterval = 20 * time.Second
	DefaultSSEWriteTimeout   = 10 * time.Second
	DefaultSSERetry          = 3 * time.Second
	DefaultSSEMaxTopics      = 16
)

var (
	errSSETopics    = fault.New("realtime_topics_required", "at least one topic query parameter is required", fault.CategoryInvalid, http.StatusBadRequest)
	errSSETooMany   = fault.New("realtime_too_many_topics", "too many topics requested", fault.CategoryInvalid, http.StatusBadRequest)
	errSSEStreaming = fault.New("realtime_streaming_unsupported", "streaming is not supported by this connection", fault.CategoryInternal, http.StatusInternalServerError)
)

// SSEOptions configures SSEHandler. Zero values select the defaults.
type SSEOptions struct {
	// Origins restricts which browser origins may open a stream. The zero
	// value allows the server's own origin only.
	Origins OriginPolicy
	// HeartbeatInterval is how often a comment line keeps the stream alive
	// through proxies and detects dead clients (default 20s).
	HeartbeatInterval time.Duration
	// WriteTimeout bounds each write to the client (default 10s). A client
	// that stops reading is dropped when a write exceeds it.
	WriteTimeout time.Duration
	// Retry is the reconnection delay suggested to EventSource clients
	// (default 3s).
	Retry time.Duration
	// MaxTopics bounds topics per stream (default 16).
	MaxTopics int
}

// SSEHandler streams events as text/event-stream. Clients choose topics with
// repeated "topic" query parameters (GET /events?topic=orders&topic=room:42)
// and resume with the Last-Event-ID header, which EventSource sends by
// itself on reconnect. The query parameter last_event_id is accepted for
// clients that cannot set headers.
//
// The handler must run behind authentication middleware; browsers'
// EventSource cannot send an Authorization header, so use
// auth.Middleware.AuthenticateCookie for browser streams. Every topic goes
// through the same Channel authorization as any other subscription, and a
// denial is answered before the stream starts.
//
// Each stream disables the server's read and write deadlines for itself
// (http.ResponseController) and applies its own per-write timeout, so
// FORGE_HTTP_WRITE_TIMEOUT does not cut streams off. A stream ends when the
// client disconnects, the broker closes, the connection is dropped as a slow
// consumer, or its lifetime elapses; EventSource then reconnects and resumes.
func SSEHandler(broker *Broker, options SSEOptions) (http.Handler, error) {
	if broker == nil {
		return nil, errors.New("realtime: broker is required")
	}
	origins, err := NewOriginChecker(options.Origins)
	if err != nil {
		return nil, err
	}
	if options.HeartbeatInterval == 0 {
		options.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if options.WriteTimeout == 0 {
		options.WriteTimeout = DefaultSSEWriteTimeout
	}
	if options.Retry == 0 {
		options.Retry = DefaultSSERetry
	}
	if options.MaxTopics == 0 {
		options.MaxTopics = DefaultSSEMaxTopics
	}
	if options.HeartbeatInterval < 0 || options.WriteTimeout < 0 || options.Retry < 0 || options.MaxTopics < 0 {
		return nil, errors.New("realtime: SSE options must not be negative")
	}
	return &sseHandler{broker: broker, options: options, origins: origins}, nil
}

type sseHandler struct {
	broker  *Broker
	options SSEOptions
	origins *OriginChecker
}

func (h *sseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		web.Error(w, r, fault.New("method_not_allowed", "method not allowed", fault.CategoryInvalid, http.StatusMethodNotAllowed))
		return
	}
	if err := h.origins.Check(r); err != nil {
		web.Error(w, r, err)
		return
	}
	if !canFlush(w) {
		web.Error(w, r, errSSEStreaming)
		return
	}
	topics, err := requestedTopics(r, h.options.MaxTopics)
	if err != nil {
		web.Error(w, r, err)
		return
	}
	var subscribeOptions []SubscribeOption
	if last := lastEventID(r); last != "" {
		// A malformed ID is ignored: the client just starts fresh.
		if id, err := uuid.Parse(last); err == nil {
			subscribeOptions = append(subscribeOptions, After(id))
		}
	}

	conn, err := h.broker.NewConn(r.Context())
	if err != nil {
		web.Error(w, r, err)
		return
	}
	defer conn.Close()
	for _, topic := range topics {
		if err := conn.Subscribe(r.Context(), topic, subscribeOptions...); err != nil {
			web.Error(w, r, err)
			return
		}
	}

	controller := http.NewResponseController(w)
	// The server-wide deadlines are meant for request/response exchanges.
	// net/http already lifts the read deadline for requests without a body,
	// but a stream must not depend on that, so it clears it explicitly. The
	// write deadline is replaced before every write (see flush); without that
	// FORGE_HTTP_WRITE_TIMEOUT would end every stream.
	if err := controller.SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		web.Logger(r.Context()).Warn("realtime: clear read deadline", "error", err)
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-store, no-transform")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	stream := &sseStream{w: w, controller: controller, timeout: h.options.WriteTimeout}
	stream.buffer.WriteString("retry: ")
	fmt.Fprintf(&stream.buffer, "%d", h.options.Retry.Milliseconds())
	stream.buffer.WriteString("\n\n")
	if stream.flush() != nil {
		return
	}

	heartbeat := time.NewTicker(h.options.HeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case event := <-conn.Events():
			stream.writeEvent(event)
			// Batch whatever else is already queued into one flush.
			for range 31 {
				select {
				case next := <-conn.Events():
					stream.writeEvent(next)
					continue
				default:
				}
				break
			}
		case <-heartbeat.C:
			stream.buffer.WriteString(": heartbeat\n\n")
		case <-conn.Done():
			return
		case <-r.Context().Done():
			return
		}
		if stream.flush() != nil {
			return
		}
	}
}

func requestedTopics(r *http.Request, limit int) ([]string, error) {
	values := r.URL.Query()["topic"]
	if len(values) == 0 {
		return nil, errSSETopics
	}
	topics := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, topic := range values {
		if !topicPattern.MatchString(topic) {
			return nil, ErrTopicInvalid
		}
		if _, dup := seen[topic]; !dup {
			seen[topic] = struct{}{}
			topics = append(topics, topic)
		}
	}
	if len(topics) > limit {
		return nil, errSSETooMany
	}
	return topics, nil
}

func lastEventID(r *http.Request) string {
	if value := r.Header.Get("Last-Event-ID"); value != "" {
		return value
	}
	return r.URL.Query().Get("last_event_id")
}

// canFlush reports whether w, following Unwrap, can flush. It is checked
// before the stream starts so an unsupported writer gets a clean error
// instead of a response that never reaches the client.
func canFlush(w http.ResponseWriter) bool {
	for {
		switch writer := w.(type) {
		case interface{ FlushError() error }:
			return true
		case http.Flusher:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			w = writer.Unwrap()
		default:
			return false
		}
	}
}

type sseStream struct {
	w          http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
	buffer     bytes.Buffer
}

// writeEvent appends one event to the buffer in text/event-stream framing.
// Synthetic events carry no id, so they never move the client's
// Last-Event-ID backwards or forwards.
func (s *sseStream) writeEvent(event Event) {
	frame, err := json.Marshal(FrameFor(event))
	if err != nil {
		return // cannot happen: Data is validated JSON
	}
	if event.ID != (uuid.UUID{}) {
		s.buffer.WriteString("id: ")
		s.buffer.WriteString(event.ID.String())
		s.buffer.WriteByte('\n')
	}
	s.buffer.WriteString("event: ")
	s.buffer.WriteString(event.Type) // validated: no line breaks possible
	s.buffer.WriteByte('\n')
	writeSSEData(&s.buffer, frame)
	s.buffer.WriteByte('\n')
}

// writeSSEData writes data as one or more "data:" lines. Line breaks in the
// payload would otherwise end the field and let a payload inject fields or
// events, so every CR, LF and CRLF starts a new data line.
func writeSSEData(buffer *bytes.Buffer, data []byte) {
	text := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(string(data))
	for line := range strings.SplitSeq(text, "\n") {
		buffer.WriteString("data: ")
		buffer.WriteString(line)
		buffer.WriteByte('\n')
	}
}

// flush writes the buffered bytes under a fresh write deadline.
func (s *sseStream) flush() error {
	defer s.buffer.Reset()
	if err := s.controller.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := s.w.Write(s.buffer.Bytes()); err != nil {
		return err
	}
	return s.controller.Flush()
}
