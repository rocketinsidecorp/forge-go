// Package websocket serves a realtime.Broker over WebSockets, using
// github.com/coder/websocket. It is an adapter: the realtime core stays free
// of third-party modules.
//
// A connection is authenticated by the middleware wrapped around Handler (use
// auth.Middleware.AuthenticateCookie for browsers), its Origin is checked
// before the upgrade (cross-site WebSocket hijacking), and every subscription
// goes through the same Channel authorization as Server-Sent Events.
//
// Protocol. Clients send JSON text messages:
//
//	{"type":"subscribe","topic":"orders","ref":"1","last_event_id":"<uuid>"}
//	{"type":"unsubscribe","topic":"orders","ref":"2"}
//	{"type":"ping","ref":"3"}
//
// ref (at most 64 characters) is echoed in the reply; last_event_id resumes
// from the replay window. The server sends realtime.Frame messages: "event",
// "subscribed", "resync", "unsubscribed", "pong" and "error" (with a stable
// code and the offending ref/topic). Any other message, a binary frame, or a
// message over the read limit is rejected; repeated violations and flooding
// close the connection.
//
// Close codes: 1001 the server is shutting down, 1008 a protocol or rate
// violation, 1009 message too big, 1013 the client could not keep up (slow
// consumer; reconnect and resume), 4401 the session expired (authenticate
// again).
package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sync"
	"time"

	ws "github.com/coder/websocket"

	"github.com/fabriciobonjorno/forge-go/fault"
	"github.com/fabriciobonjorno/forge-go/realtime"
	"github.com/fabriciobonjorno/forge-go/uuid"
	"github.com/fabriciobonjorno/forge-go/web"
)

// Defaults applied to zero Options fields.
const (
	DefaultReadLimit         = 4 << 10
	DefaultPingInterval      = 25 * time.Second
	DefaultPingTimeout       = 10 * time.Second
	DefaultWriteTimeout      = 10 * time.Second
	DefaultMessagesPerSecond = 20
	maxInvalidMessages       = 10

	// StatusSessionExpired closes a connection whose session lapsed.
	StatusSessionExpired ws.StatusCode = 4401
)

var refPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{0,64}$`)

// Options configures Handler. Zero values select the defaults.
type Options struct {
	// Origins restricts which browser origins may connect. The zero value
	// allows the server's own origin only.
	Origins realtime.OriginPolicy
	// ReadLimit is the largest message a client may send (default 4 KiB).
	// Client messages are small control messages; larger ones are refused.
	ReadLimit int64
	// PingInterval and PingTimeout drive the keepalive: the server pings every
	// interval and closes a connection that does not answer within the
	// timeout (defaults 25s and 10s).
	PingInterval time.Duration
	PingTimeout  time.Duration
	// WriteTimeout bounds each write to the client (default 10s).
	WriteTimeout time.Duration
	// MessagesPerSecond caps client messages (default 20, burst twice that).
	MessagesPerSecond int
}

type handler struct {
	broker  *realtime.Broker
	options Options
	origins *realtime.OriginChecker
}

// Handler upgrades requests to WebSockets bound to broker. It must run behind
// authentication middleware.
func Handler(broker *realtime.Broker, options Options) (http.Handler, error) {
	if broker == nil {
		return nil, errors.New("websocket: broker is required")
	}
	origins, err := realtime.NewOriginChecker(options.Origins)
	if err != nil {
		return nil, err
	}
	if options.ReadLimit == 0 {
		options.ReadLimit = DefaultReadLimit
	}
	if options.PingInterval == 0 {
		options.PingInterval = DefaultPingInterval
	}
	if options.PingTimeout == 0 {
		options.PingTimeout = DefaultPingTimeout
	}
	if options.WriteTimeout == 0 {
		options.WriteTimeout = DefaultWriteTimeout
	}
	if options.MessagesPerSecond == 0 {
		options.MessagesPerSecond = DefaultMessagesPerSecond
	}
	if options.ReadLimit < 0 || options.PingInterval < 0 || options.PingTimeout < 0 || options.WriteTimeout < 0 || options.MessagesPerSecond < 0 {
		return nil, errors.New("websocket: options must not be negative")
	}
	return &handler{broker: broker, options: options, origins: origins}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		web.Error(w, r, fault.New("method_not_allowed", "method not allowed", fault.CategoryInvalid, http.StatusMethodNotAllowed))
		return
	}
	// Before the upgrade, so a refused client gets an ordinary HTTP error.
	if err := h.origins.Check(r); err != nil {
		web.Error(w, r, err)
		return
	}
	conn, err := h.broker.NewConn(r.Context())
	if err != nil {
		web.Error(w, r, err)
		return
	}
	defer conn.Close()

	socket, err := ws.Accept(w, r, &ws.AcceptOptions{
		// The Origin was checked above with Forge's stricter policy; this
		// only disables the library's own host comparison.
		InsecureSkipVerify: true,
		CompressionMode:    ws.CompressionDisabled,
	})
	if err != nil {
		return // Accept has already written the response
	}
	defer func() { _ = socket.CloseNow() }()
	socket.SetReadLimit(h.options.ReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s := &session{h: h, conn: conn, socket: socket, ctx: ctx}
	var wg sync.WaitGroup
	wg.Go(s.writeLoop)
	s.readLoop()
	cancel()
	_ = socket.CloseNow()
	wg.Wait()
}

type session struct {
	h      *handler
	conn   *realtime.Conn
	socket *ws.Conn
	ctx    context.Context
}

// writeLoop sends queued events, keepalive pings, and the closing status.
func (s *session) writeLoop() {
	ping := time.NewTicker(s.h.options.PingInterval)
	defer ping.Stop()
	for {
		select {
		case event := <-s.conn.Events():
			if err := s.send(realtime.FrameFor(event)); err != nil {
				return
			}
		case <-s.conn.Done():
			code, reason := closeStatus(s.conn.Err())
			_ = s.socket.Close(code, reason)
			return
		case <-ping.C:
			pingCtx, cancel := context.WithTimeout(s.ctx, s.h.options.PingTimeout)
			err := s.socket.Ping(pingCtx)
			cancel()
			if err != nil {
				if s.ctx.Err() == nil {
					_ = s.socket.Close(ws.StatusGoingAway, "ping timeout")
				}
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

func closeStatus(reason error) (ws.StatusCode, string) {
	switch {
	case errors.Is(reason, realtime.ErrSlowConsumer):
		return ws.StatusTryAgainLater, "too slow; reconnect and resume"
	case errors.Is(reason, realtime.ErrSessionExpired):
		return StatusSessionExpired, "session expired"
	default:
		return ws.StatusGoingAway, "server shutting down"
	}
}

func (s *session) send(frame realtime.Frame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.h.options.WriteTimeout)
	defer cancel()
	// A write that times out closes the connection (library behavior), which
	// is how a client that stops reading is dropped.
	return s.socket.Write(ctx, ws.MessageText, payload)
}

type clientMessage struct {
	Type        string `json:"type"`
	Topic       string `json:"topic"`
	Ref         string `json:"ref"`
	LastEventID string `json:"last_event_id"`
}

func (s *session) readLoop() {
	limiter := newLimiter(s.h.options.MessagesPerSecond)
	invalid := 0
	for {
		kind, data, err := s.socket.Read(s.ctx)
		if err != nil {
			return
		}
		if kind != ws.MessageText {
			_ = s.socket.Close(ws.StatusUnsupportedData, "text messages only")
			return
		}
		if !limiter.allow(time.Now()) {
			_ = s.socket.Close(ws.StatusPolicyViolation, "rate limit exceeded")
			return
		}
		if !s.handle(data) {
			if invalid++; invalid >= maxInvalidMessages {
				_ = s.socket.Close(ws.StatusPolicyViolation, "too many invalid messages")
				return
			}
		}
	}
}

var (
	errBadMessage = fault.New("invalid_message", "message must be a JSON object with a known type", fault.CategoryInvalid, 0)
	errBadRef     = fault.New("invalid_ref", "ref must be at most 64 characters of [A-Za-z0-9_.:-]", fault.CategoryInvalid, 0)
)

// handle processes one client message and reports whether it was valid.
func (s *session) handle(data []byte) bool {
	var message clientMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil || decoder.More() {
		s.reject(clientMessage{}, errBadMessage)
		return false
	}
	if !refPattern.MatchString(message.Ref) {
		s.reject(clientMessage{}, errBadRef)
		return false
	}
	switch message.Type {
	case "ping":
		_ = s.send(realtime.Frame{Type: "pong", Ref: message.Ref})
		return true
	case "subscribe":
		var options []realtime.SubscribeOption
		if message.LastEventID != "" {
			id, err := uuid.Parse(message.LastEventID)
			if err != nil {
				s.reject(message, errBadMessage)
				return false
			}
			options = append(options, realtime.After(id))
		}
		if err := s.conn.Subscribe(s.ctx, message.Topic, options...); err != nil {
			s.reject(message, err)
			return true // a denied subscription is not a protocol violation
		}
		return true
	case "unsubscribe":
		if !realtime.ValidTopic(message.Topic) {
			s.reject(message, realtime.ErrTopicInvalid)
			return false
		}
		s.conn.Unsubscribe(message.Topic)
		_ = s.send(realtime.Frame{Type: "unsubscribed", Topic: message.Topic, Ref: message.Ref})
		return true
	default:
		s.reject(message, errBadMessage)
		return false
	}
}

// reject answers with an error frame carrying only the public code and
// message of err.
func (s *session) reject(message clientMessage, err error) {
	public := fault.From(err)
	if public == nil {
		public = fault.New("internal_error", "internal server error", fault.CategoryInternal, http.StatusInternalServerError)
		web.Logger(s.ctx).Error("realtime websocket request failed", "error", err)
	}
	_ = s.send(realtime.Frame{Type: "error", Code: public.Code, Message: public.Message, Ref: message.Ref, Topic: safeTopic(message.Topic)})
}

// safeTopic echoes a topic back only if it is a valid topic name, so errors
// cannot reflect arbitrary client input.
func safeTopic(topic string) string {
	if realtime.ValidTopic(topic) {
		return topic
	}
	return ""
}

// limiter is a token bucket: rate tokens per second, burst of twice that.
type limiter struct {
	rate, burst float64
	tokens      float64
	last        time.Time
}

func newLimiter(perSecond int) *limiter {
	burst := float64(perSecond) * 2
	return &limiter{rate: float64(perSecond), burst: burst, tokens: burst}
}

func (l *limiter) allow(now time.Time) bool {
	if !l.last.IsZero() {
		l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
