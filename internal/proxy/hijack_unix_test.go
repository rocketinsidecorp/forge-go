//go:build linux || darwin || freebsd || netbsd || openbsd

package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// smallBuffers shrinks the send buffer of every accepted connection, so a
// client that reads nothing fills it quickly.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(8 << 10)
	}
	return conn, err
}

// Regression: a client pipelines a response it never reads and then a
// WebSocket upgrade. The first response fits in the socket buffers, the
// 101 response does not; it is written on the hijacked connection, which
// must keep a write deadline, or the handler blocks for ever holding the
// connection, the upstream connection and the target lease (a drain would
// wait for it). The sizes sweep the window where the 101 is the write that
// blocks.
func TestUpgradeAfterAnUnreadResponseDoesNotHang(t *testing.T) {
	app := newBackend(t, "app", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			size, _ := strconv.Atoi(r.URL.Query().Get("size"))
			w.Header().Set("Content-Length", strconv.Itoa(size))
			_, _ = w.Write(bytes.Repeat([]byte("x"), size))
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(io.Discard, rw)
	})
	srv, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	if err != nil {
		t.Fatal(err)
	}
	srv.idleTimeout = 300 * time.Millisecond
	front := httptest.NewUnstartedServer(srv)
	front.Listener = smallBuffers{front.Listener}
	front.Start()
	t.Cleanup(front.Close)
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	dialer := net.Dialer{Timeout: 5 * time.Second, Control: smallReceiveBuffer}
	addr := front.Listener.Addr().String()
	var conns []net.Conn
	t.Cleanup(func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	for size := 0; size <= 48<<10; size += 96 {
		conn, err := dialer.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		requests := fmt.Sprintf("GET /big?size=%d HTTP/1.1\r\nHost: %s\r\n\r\n"+
			"GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", size, testHost, testHost)
		if _, err := io.WriteString(conn, requests); err != nil {
			t.Fatal(err)
		}
	}
	// Every exchange ends within a few idle timeouts: none holds the
	// target.
	waitInFlight(t, srv, 0, 10*time.Second)
}

// The 101 response written through the hijacked connection's buffered
// writer is bounded by the idle timeout, not blocked for ever.
func TestHijackedConnectionKeepsAWriteDeadline(t *testing.T) {
	proxySide, clientSide := net.Pipe()
	t.Cleanup(func() { _ = proxySide.Close(); _ = clientSide.Close() })
	writer := &hijackable{conn: proxySide}
	recorder := &statusRecorder{ResponseWriter: writer, controller: http.NewResponseController(writer), idle: 200 * time.Millisecond}
	_ = proxySide.SetWriteDeadline(time.Now().Add(time.Hour))
	conn, rw, err := recorder.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := conn.(*idleConn); !ok {
		t.Fatalf("hijacked connection %T has no idle timeout", conn)
	}
	done := make(chan error, 1)
	go func() {
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n\r\n")
		done <- rw.Flush()
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("flush to a client that reads nothing succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("101 flush blocked past the idle timeout")
	}
}

// hijackable is a ResponseWriter whose connection is a pipe.
type hijackable struct {
	http.ResponseWriter
	conn net.Conn
}

func (h *hijackable) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

func smallReceiveBuffer(_, _ string, raw syscall.RawConn) error {
	var sockErr error
	err := raw.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<10)
	})
	if err != nil {
		return err
	}
	return sockErr
}

// A client that reads slowly but steadily (2 KiB every 100 ms) receives the
// whole 64 KiB response, which takes over three seconds: the budget covers
// progress, not whole writes. A 32 KiB write alone takes 1.6 s at that pace,
// longer than the one second idle timeout, so only a budget that counts
// progress lets it through; and the timeout is ten times the pause between
// reads, so a loaded machine that stretches a pause does not look like a
// stalled client.
func TestSlowClientThatKeepsReadingIsNotCutOff(t *testing.T) {
	const size = 64 << 10
	app := newBackend(t, "app", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = w.Write(bytes.Repeat([]byte("x"), size))
	})
	srv, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	if err != nil {
		t.Fatal(err)
	}
	srv.idleTimeout = time.Second
	front := httptest.NewUnstartedServer(srv)
	front.Listener = smallBuffers{front.Listener}
	front.Start()
	t.Cleanup(front.Close)
	if _, err := srv.Deploy(t.Context(), fastRequest(app.addr())); err != nil {
		t.Fatal(err)
	}
	dialer := net.Dialer{Timeout: 5 * time.Second, Control: smallReceiveBuffer}
	conn, err := dialer.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "GET /slow HTTP/1.1\r\nHost: "+testHost+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	var received []byte
	buffer := make([]byte, 2<<10)
	for {
		time.Sleep(100 * time.Millisecond)
		n, err := io.ReadFull(conn, buffer)
		received = append(received, buffer[:n]...)
		if err != nil {
			break
		}
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(received)), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) != size {
		t.Fatalf("received %d of %d bytes: %v", len(body), size, err)
	}
}

// A tunnel whose client side is busy receiving one long write that keeps
// progressing is not idle: the read side, waiting for client data, sees
// that activity chunk by chunk.
//
// It runs on the bubble's fake clock: the client takes 100 ms per chunk
// against a 200 ms idle timeout, a margin that real time cannot guarantee on
// a loaded runner (a descheduled client made the write, or the read, time
// out) but that fake time keeps exactly.
func TestIdleConnCountsAProgressingWriteAsActivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const idle, chunkEvery = 200 * time.Millisecond, 100 * time.Millisecond
		proxySide, clientSide := net.Pipe()
		stop := make(chan struct{})
		defer func() {
			// Both goroutines return, so a failed assertion is reported as
			// itself, not as a leaked goroutine.
			close(stop)
			_ = proxySide.Close()
			_ = clientSide.Close()
			synctest.Wait()
		}()
		conn := newIdleConn(proxySide, idle)
		readErr := make(chan error, 1)
		go func() {
			_, err := conn.Read(make([]byte, 1))
			readErr <- err
		}()
		go func() {
			buffer := make([]byte, writeChunk)
			for {
				select {
				case <-time.After(chunkEvery):
				case <-stop:
					return
				}
				if _, err := io.ReadFull(clientSide, buffer); err != nil {
					return
				}
			}
		}()
		// 16 chunks at 100 ms each: well past the idle timeout in total.
		start := time.Now()
		if _, err := conn.Write(make([]byte, 16*writeChunk)); err != nil {
			t.Fatalf("progressing write failed: %v", err)
		}
		if took := time.Since(start); took < 4*idle {
			t.Fatalf("the write took %v: not long enough to outlast the idle timeout", took)
		}
		select {
		case err := <-readErr:
			t.Fatalf("read gave up during the write: %v", err)
		default:
		}
		// Once nothing moves, the read gives up one idle timeout after the
		// last chunk, and not before.
		finished := time.Now()
		if err := <-readErr; err == nil {
			t.Fatal("read returned without data or error")
		}
		if waited := time.Since(finished); waited < idle || waited > 2*idle {
			t.Fatalf("the read gave up %v after the last chunk, want about %v", waited, idle)
		}
	})
}
