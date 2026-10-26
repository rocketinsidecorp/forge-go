package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
)

func TestLimitListenerAdmitsBeforeAcceptAndFreesSlotsOnClose(t *testing.T) {
	t.Parallel()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newLimitListener(tcp, 2)
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- conn
		}
	}()
	dial := func() net.Conn {
		conn, err := net.Dial("tcp", tcp.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	for range 3 {
		dial() // the third connects (kernel backlog) but is not accepted
	}
	first, second := <-accepted, <-accepted
	select {
	case <-accepted:
		t.Fatal("a third connection was accepted over the limit of 2")
	case <-time.After(200 * time.Millisecond):
	}
	// Closing twice frees one slot, not two.
	_ = first.Close()
	_ = first.Close()
	third := <-accepted
	dial()
	select {
	case <-accepted:
		t.Fatal("a closed connection freed more than one slot")
	case <-time.After(200 * time.Millisecond):
	}
	_ = second.Close()
	_ = third.Close()
	if conn := <-accepted; conn == nil {
		t.Fatal("a freed slot did not admit the waiting connection")
	}
}

func TestLimitListenerCloseWakesAWaitingAccept(t *testing.T) {
	t.Parallel()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newLimitListener(tcp, 1)
	conn, err := net.Dial("tcp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := listener.Accept(); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := listener.Accept(); result <- err }()
	time.Sleep(50 * time.Millisecond) // real-clock: a second Accept must be blocked inside the listener before it is closed
	_ = listener.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Accept succeeded on a closed listener")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept stayed blocked after Close")
	}
}

func TestRunRefusesOutOfRangeLimits(t *testing.T) {
	for _, tc := range []struct{ flag, value, want string }{
		{"-max-header-bytes", "100", "-max-header-bytes must be between"},
		{"-max-header-bytes", "2097152", "-max-header-bytes must be between"},
		{"-max-connections", "0", "-max-connections must be at least 1"},
	} {
		var stdout, stderr bytes.Buffer
		code := Main(t.Context(), []string{"run", tc.flag, tc.value, "-state", ""}, &stdout, &stderr)
		if code != ExitFailed || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s %s: exit %d: %s", tc.flag, tc.value, code, stderr.String())
		}
	}
}

func readStatus(t *testing.T, conn net.Conn, wait time.Duration) (int, bool) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, false
	}
	_ = resp.Body.Close()
	return resp.StatusCode, true
}

// run serves with -max-header-bytes and -max-connections: a header over the
// limit is refused with 431, and with one connection allowed a second client
// is served only after the first goes away.
func TestRunAppliesHeaderAndConnectionLimits(t *testing.T) {
	dir, err := os.MkdirTemp("", "fpl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	addr := freeAddr(t)
	socket := filepath.Join(dir, "c.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, []string{"run", "-http-addr", addr, "-https-addr", "", "-control", socket, "-state", "",
			"-max-header-bytes", "4096", "-max-connections", "1"}, &stdout, &stderr)
	}()
	if !waittest.Poll(5*time.Second, func() bool {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
		}
		return err == nil
	}) {
		t.Fatalf("proxy did not start: %s", stderr.String())
	}

	request := func(header string) string {
		return "GET / HTTP/1.1\r\nHost: unknown.example\r\n" + header + "\r\n"
	}
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	// 10 KiB of headers is over the 4 KiB limit.
	if _, err := io.WriteString(first, request("X-Big: "+strings.Repeat("a", 10<<10)+"\r\n")); err != nil {
		t.Fatal(err)
	}
	if status, ok := readStatus(t, first, 5*time.Second); !ok || status != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("oversized headers: status %d, answered %v", status, ok)
	}
	_ = first.Close()

	// One connection holds the only slot; the second is not served.
	holder, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(holder, request("")); err != nil {
		t.Fatal(err)
	}
	if _, ok := readStatus(t, holder, 5*time.Second); !ok {
		t.Fatal("the connection holding the slot was not served")
	}
	waiting, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = waiting.Close() }()
	if _, err := io.WriteString(waiting, request("")); err != nil {
		t.Fatal(err)
	}
	if status, ok := readStatus(t, waiting, 300*time.Millisecond); ok {
		t.Fatalf("a second connection was served over -max-connections 1 (status %d)", status)
	}
	_ = holder.Close()
	if _, ok := readStatus(t, waiting, 5*time.Second); !ok {
		t.Fatal("the waiting connection was not served after the slot was freed")
	}

	cancel()
	if code := <-done; code != ExitOK {
		t.Fatalf("run exit %d: %s", code, stderr.String())
	}
}
