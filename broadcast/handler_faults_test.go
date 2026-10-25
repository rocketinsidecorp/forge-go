package broadcast_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/broadcast"
)

var errScripted = errors.New("scripted failure")

// scriptedWriter is a response writer whose writes, flushes and deadlines
// fail on demand, as a client that went away or a connection that cannot
// carry deadlines would.
type scriptedWriter struct {
	header           http.Header
	status           int
	body             bytes.Buffer
	writes           int
	failWriteAfter   int // the write with this number (from 1) and every later one fail; 0 never
	flushErr         error
	readDeadlineErr  error
	writeDeadlineErr func(time.Time) error
}

func (w *scriptedWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *scriptedWriter) WriteHeader(status int) { w.status = status }
func (w *scriptedWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failWriteAfter > 0 && w.writes >= w.failWriteAfter {
		return 0, errScripted
	}
	return w.body.Write(p)
}
func (w *scriptedWriter) FlushError() error { return w.flushErr }
func (w *scriptedWriter) SetReadDeadline(time.Time) error {
	return w.readDeadlineErr
}
func (w *scriptedWriter) SetWriteDeadline(t time.Time) error {
	if w.writeDeadlineErr != nil {
		return w.writeDeadlineErr(t)
	}
	return nil
}

type spySubscription struct {
	messages chan broadcast.Message
	err      error
	closed   atomic.Bool
}

func (s *spySubscription) Messages() <-chan broadcast.Message { return s.messages }
func (s *spySubscription) Err() error                         { return s.err }
func (s *spySubscription) Close()                             { s.closed.Store(true) }

type spyBroker struct {
	subscription *spySubscription
	err          error
	subscribed   atomic.Int32
}

func (b *spyBroker) Publish(context.Context, broadcast.Channel, broadcast.Message) error { return nil }
func (b *spyBroker) Subscribe(context.Context, broadcast.Channel) (broadcast.Subscription, error) {
	b.subscribed.Add(1)
	if b.err != nil {
		return nil, b.err
	}
	return b.subscription, nil
}

type streamRig struct {
	broker  *spyBroker
	handler http.Handler
	request *http.Request
	logs    *bytes.Buffer
}

func newStreamRig(t *testing.T, mutate func(*broadcast.HandlerOptions)) *streamRig {
	t.Helper()
	signer, _ := newSigner(t, nil)
	channel, err := broadcast.Global("room")
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(channel)
	if err != nil {
		t.Fatal(err)
	}
	broker := &spyBroker{subscription: &spySubscription{messages: make(chan broadcast.Message, 4)}}
	logs := &bytes.Buffer{}
	opts := broadcast.HandlerOptions{
		Broker: broker, Signer: signer, Heartbeat: time.Hour,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mutate != nil {
		mutate(&opts)
	}
	handler, err := broadcast.NewHandler(opts)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/streams?stream="+url.QueryEscape(token), nil)
	return &streamRig{broker: broker, handler: handler, request: request, logs: logs}
}

func TestHandlerReportsASubscribeFailureAndAConnectionThatCannotClearItsDeadline(t *testing.T) {
	t.Parallel()
	rig := newStreamRig(t, nil)
	rig.broker.err = errScripted
	recorder := httptest.NewRecorder()
	rig.handler.ServeHTTP(recorder, rig.request)
	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("a broker that cannot subscribe gave %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "scripted failure") {
		t.Errorf("the broker's error reached the client: %s", recorder.Body.String())
	}

	rig = newStreamRig(t, nil)
	writer := &scriptedWriter{readDeadlineErr: errScripted}
	rig.handler.ServeHTTP(writer, rig.request)
	if writer.status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the read deadline cannot be cleared", writer.status)
	}
	if rig.broker.subscribed.Load() != 0 {
		t.Error("the handler subscribed on a connection it could not configure")
	}
}

// Any failure to write ends the stream and releases the subscription: a
// stalled or departed client never holds one.
func TestHandlerEndsTheStreamWhenAWriteFails(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		writer *scriptedWriter
		// messages are queued before the handler runs.
		messages  []string
		heartbeat time.Duration
		// writes is how many writes reached the writer before the stream ended.
		writes int
	}{
		"deadline cannot be set": {writer: &scriptedWriter{writeDeadlineErr: func(time.Time) error { return errScripted }}, writes: 0},
		"first write fails":      {writer: &scriptedWriter{failWriteAfter: 1}, writes: 1},
		"flush fails":            {writer: &scriptedWriter{flushErr: errScripted}, writes: 1},
		"deadline cannot be cleared": {writer: &scriptedWriter{writeDeadlineErr: func(t time.Time) error {
			if t.IsZero() {
				return errScripted
			}
			return nil
		}}, writes: 1},
		"a message cannot be written":   {writer: &scriptedWriter{failWriteAfter: 2}, messages: []string{"hello"}, writes: 2},
		"a heartbeat cannot be written": {writer: &scriptedWriter{failWriteAfter: 2}, heartbeat: 5 * time.Millisecond, writes: 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newStreamRig(t, func(o *broadcast.HandlerOptions) {
				if tc.heartbeat > 0 {
					o.Heartbeat = tc.heartbeat
				}
			})
			for _, message := range tc.messages {
				rig.broker.subscription.messages <- broadcast.Message{Data: message}
			}
			done := make(chan struct{})
			go func() { rig.handler.ServeHTTP(tc.writer, rig.request); close(done) }()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("the stream kept running after a write failed")
			}
			if !rig.broker.subscription.closed.Load() {
				t.Error("the subscription was not released")
			}
			if tc.writer.writes != tc.writes {
				t.Errorf("%d writes reached the client, want %d", tc.writer.writes, tc.writes)
			}
		})
	}
}

func TestHandlerLogsWhyASubscriptionEnded(t *testing.T) {
	t.Parallel()
	rig := newStreamRig(t, nil)
	rig.broker.subscription.err = broadcast.ErrSlowConsumer
	close(rig.broker.subscription.messages)
	writer := &scriptedWriter{}
	rig.handler.ServeHTTP(writer, rig.request)
	if !strings.Contains(rig.logs.String(), "event stream ended") || !strings.Contains(rig.logs.String(), broadcast.ErrSlowConsumer.Error()) {
		t.Errorf("log = %q, want the reason a slow consumer was dropped", rig.logs.String())
	}
	if !strings.Contains(writer.body.String(), ": connected") || writer.status != http.StatusOK {
		t.Errorf("the stream did not open first: status=%d body=%q", writer.status, writer.body.String())
	}

	// A broker that shut down is the normal end: nothing to log.
	rig = newStreamRig(t, nil)
	rig.broker.subscription.err = broadcast.ErrClosed
	close(rig.broker.subscription.messages)
	rig.handler.ServeHTTP(&scriptedWriter{}, rig.request)
	if strings.Contains(rig.logs.String(), "event stream ended") {
		t.Errorf("a broker shutdown was logged as an error: %q", rig.logs.String())
	}
}
