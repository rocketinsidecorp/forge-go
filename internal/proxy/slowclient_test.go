package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A request routed to a service that is removed before it acquires the
// target must get 503 promptly instead of spinning on the retired target.
func TestRequestRacingRemoveGets503(t *testing.T) {
	app := newBackend(t, "app", nil)
	srv, _ := newTestServer(t, "")
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	srv.mu.RLock()
	svc := srv.services["app"]
	srv.mu.RUnlock()
	// The window between routing and acquire: the target is retired while
	// the service still points at it.
	svc.target.Load().retire()
	done := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		srv.serve(recorder, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil), svc)
		done <- recorder.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request spins on a retired target")
	}
	// Remove itself leaves the routed service without a target.
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	srv.mu.RLock()
	svc = srv.services["app"]
	srv.mu.RUnlock()
	if err := srv.Remove("app", time.Second); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	srv.serve(recorder, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil), svc)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("after Remove: %d", recorder.Code)
	}
}

func slowBodyServer(t *testing.T) (*backend, *httptest.Server, chan string) {
	t.Helper()
	received := make(chan string, 4)
	app := newBackend(t, "app", func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		received <- strconv.FormatInt(r.ContentLength, 10) + "/" + strconv.FormatInt(n, 10)
		if err == nil {
			_, _ = io.WriteString(w, "ok")
		}
	})
	srv, front := newTestServer(t, "")
	srv.bodyTimeout = 200 * time.Millisecond
	req := fastRequest(app.addr())
	req.BufferBytes = 1024
	if _, err := srv.Deploy(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	return app, front, received
}

// postChunked sends body without Content-Length, writing chunks with a
// pause between them, and returns the status and elapsed time.
func postChunked(t *testing.T, front string, chunks [][]byte, pause time.Duration) (int, time.Duration) {
	t.Helper()
	reader, writer := io.Pipe()
	go func() {
		for _, chunk := range chunks {
			if _, err := writer.Write(chunk); err != nil {
				return
			}
			time.Sleep(pause)
		}
		_ = writer.Close()
	}()
	request, _ := http.NewRequest(http.MethodPost, front+"/upload", reader)
	request.Host = testHost
	start := time.Now()
	response, err := http.DefaultClient.Do(request)
	_ = reader.CloseWithError(io.ErrClosedPipe)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return response.StatusCode, time.Since(start)
}

func TestSlowChunkedBodyIsCutOffAtDeadline(t *testing.T) {
	app, front, _ := slowBodyServer(t)
	chunks := make([][]byte, 40)
	for i := range chunks {
		chunks[i] = []byte("x")
	}
	// 40 bytes, one every 50ms: well under the buffer, but 2s in total.
	status, elapsed := postChunked(t, front.URL, chunks, 50*time.Millisecond)
	if status != http.StatusRequestTimeout || elapsed > 1500*time.Millisecond {
		t.Fatalf("slow chunked body: status %d after %s, want 408 at the deadline", status, elapsed)
	}
	if app.hits.Load() != 0 {
		t.Fatal("slow body reached the application")
	}
}

func TestChunkedBodiesAreBufferedOrStreamed(t *testing.T) {
	app, front, received := slowBodyServer(t)
	// Small chunked body: buffered and forwarded with a Content-Length.
	if status, _ := postChunked(t, front.URL, [][]byte{[]byte("hello "), []byte("world")}, time.Millisecond); status != 200 {
		t.Fatalf("small chunked body: %d", status)
	}
	if got := <-received; got != "11/11" {
		t.Fatalf("small chunked body arrived as %s, want Content-Length 11", got)
	}
	// Larger than the buffer: streamed while data keeps arriving.
	big := [][]byte{bytes.Repeat([]byte("a"), 1000), bytes.Repeat([]byte("b"), 1000), bytes.Repeat([]byte("c"), 1000)}
	if status, _ := postChunked(t, front.URL, big, 50*time.Millisecond); status != 200 {
		t.Fatalf("streamed chunked body: %d", status)
	}
	if got := <-received; got != "-1/3000" {
		t.Fatalf("streamed body arrived as %s", got)
	}
	// Larger than the buffer, then stalled: cut off by the per-read deadline.
	stalled := [][]byte{bytes.Repeat([]byte("d"), 2000), []byte("late")}
	status, elapsed := postChunked(t, front.URL, stalled, 2*time.Second)
	if status != http.StatusRequestTimeout || elapsed > 1500*time.Millisecond {
		t.Fatalf("stalled streamed body: status %d after %s", status, elapsed)
	}
	if got := <-received; strings.HasSuffix(got, "/2004") {
		t.Fatalf("stalled body completed: %s", got)
	}
	_ = app
}
