package telemetry_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// serve runs request through the server middleware around next and returns
// the span it recorded.
func serve(t *testing.T, next http.Handler, request *http.Request) (telemetry.SpanData, *httptest.ResponseRecorder) {
	t.Helper()
	sink := &spanSink{}
	tel := newTelemetry(t, sink, false)
	middleware, err := tel.ServerMiddleware()
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	middleware(next).ServeHTTP(recorder, request)
	flush(t, tel)
	spans := sink.all()
	if len(spans) != 1 {
		t.Fatalf("spans: %d", len(spans))
	}
	return spans[0], recorder
}

func TestServerSpanRecordsSchemeAndProtocolVersion(t *testing.T) {
	t.Parallel()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	for name, tc := range map[string]struct {
		tls          bool
		major, minor int
		scheme, ver  string
	}{
		"http 1.1":  {false, 1, 1, "http", "1.1"},
		"https 1.1": {true, 1, 1, "https", "1.1"},
		"http 1.0":  {false, 1, 0, "http", "1.0"},
		"h2":        {true, 2, 0, "https", "2"},
		"h3":        {true, 3, 0, "https", "3"},
		"http 0.9":  {false, 0, 9, "http", "0.9"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.ProtoMajor, request.ProtoMinor = tc.major, tc.minor
			if tc.tls {
				request.TLS = &tls.ConnectionState{}
			}
			span, _ := serve(t, next, request)
			if v, _ := attr(span, "url.scheme"); v.AsString() != tc.scheme {
				t.Errorf("url.scheme = %q, want %q", v.AsString(), tc.scheme)
			}
			if v, _ := attr(span, "network.protocol.version"); v.AsString() != tc.ver {
				t.Errorf("network.protocol.version = %q, want %q", v.AsString(), tc.ver)
			}
		})
	}
}

func TestServerSpanTakesTheStatusOfAnImplicitOKWrite(t *testing.T) {
	t.Parallel()
	span, recorder := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("body")) // no WriteHeader: net/http answers 200
	}), httptest.NewRequest(http.MethodGet, "/", nil))
	if v, _ := attr(span, "http.response.status_code"); v.AsInt64() != 200 || recorder.Body.String() != "body" {
		t.Errorf("status %d body %q", v.AsInt64(), recorder.Body)
	}
}

func TestServerMiddlewareForwardsHijackAndRecordsSwitchingProtocols(t *testing.T) {
	t.Parallel()
	sink := &spanSink{}
	tel := newTelemetry(t, sink, false)
	middleware, err := tel.ServerMiddleware()
	if err != nil {
		t.Fatal(err)
	}
	// A recorder cannot be hijacked, and the refusal reaches the handler.
	var hijackErr error
	middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _, hijackErr = http.NewResponseController(w).Hijack()
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(hijackErr, http.ErrNotSupported) {
		t.Fatalf("hijack on a recorder: %v", hijackErr)
	}

	server := httptest.NewServer(middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
		_ = rw.Flush()
	})))
	defer server.Close()
	response, err := http.Get(server.URL + "/upgrade")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	if line, _ := reader.ReadString(0); line != "ok" {
		t.Errorf("hijacked body = %q", line)
	}
	_ = response.Body.Close()
	// Close does not wait for a hijacked connection's handler: its span ends
	// when the middleware returns, so flush until both spans are exported.
	server.Close()
	waittest.Until(t, 5*time.Second, "both spans exported", func() bool {
		flush(t, tel)
		return len(sink.all()) >= 2
	})
	var statuses []int64
	for _, span := range sink.all() {
		if v, ok := attr(span, "http.response.status_code"); ok {
			statuses = append(statuses, v.AsInt64())
		}
	}
	if len(statuses) != 2 || statuses[0] != 200 || statuses[1] != http.StatusSwitchingProtocols {
		t.Errorf("recorded statuses %v, want [200 101]", statuses)
	}
}

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

type statusTransport int

func (s statusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: int(s), Body: http.NoBody, Header: http.Header{}, Request: r}, nil
}

func TestTransportRecordsFailuresAndErrorStatuses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		base     http.RoundTripper
		wantErr  bool
		wantType string
		status   string
	}{
		"transport error": {failingTransport{errors.New("connection refused")}, true, "*errors.errorString", "error"},
		"server error":    {statusTransport(http.StatusBadGateway), false, "502", "error"},
		"client error":    {statusTransport(http.StatusNotFound), false, "404", "error"},
		"redirect":        {statusTransport(http.StatusFound), false, "", "unset"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sink := &spanSink{}
			tel := newTelemetry(t, sink, false)
			transport, err := tel.Transport(tc.base)
			if err != nil {
				t.Fatal(err)
			}
			ctx, parent := tel.Tracer("t").Start(context.Background(), "parent", telemetry.SpanKindInternal)
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.example:8081/x", nil)
			response, err := transport.RoundTrip(request)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if response != nil {
				_ = response.Body.Close()
			}
			parent.End()
			flush(t, tel)
			var client telemetry.SpanData
			for _, span := range sink.all() {
				if span.Kind == telemetry.SpanKindClient {
					client = span
				}
			}
			if client.Name == "" {
				t.Fatal("no client span")
			}
			if v, _ := attr(client, "server.address"); v.AsString() != "upstream.example" {
				t.Errorf("server.address = %q", v.AsString())
			}
			if v, _ := attr(client, "server.port"); v.AsInt64() != 8081 {
				t.Errorf("server.port = %d", v.AsInt64())
			}
			v, present := attr(client, "error.type")
			if present != (tc.wantType != "") || v.AsString() != tc.wantType {
				t.Errorf("error.type = %q (present %v), want %q", v.AsString(), present, tc.wantType)
			}
			if client.Status.String() != tc.status {
				t.Errorf("status = %v, want %s", client.Status, tc.status)
			}
			if tc.wantErr && !strings.Contains(client.Name, "GET") {
				t.Errorf("span name %q", client.Name)
			}
		})
	}
}

func TestTransportRefusesARequestWithoutAURL(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil, false)
	transport, err := tel.Transport(failingTransport{errors.New("must not be reached")})
	if err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]*http.Request{"nil": nil, "no URL": {Method: http.MethodGet}} {
		response, err := transport.RoundTrip(request)
		if response != nil || err == nil || !strings.Contains(err.Error(), "URL is required") {
			t.Errorf("%s: response %v err %v", name, response, err)
		}
	}
}
