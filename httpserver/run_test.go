package httpserver_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/httpserver"
	"github.com/rocketinsidecorp/forge-go/internal/waittest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// anyAddress asks the kernel for a port when Run binds it. A port found free
// by a probe and released could be taken by another process before Run binds
// it; port 0 leaves no such window. Run logs the address it bound, and
// listenAddress reads it from there.
const anyAddress = "127.0.0.1:0"

// logBuffer is a slog output safe for Run's writes and a test's reads.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newLogs() (*slog.Logger, *logBuffer) {
	logs := &logBuffer{}
	return slog.New(slog.NewTextHandler(logs, nil)), logs
}

var servingHTTP = regexp.MustCompile(`msg="serving HTTP" address=(\S+)`)

// listenAddress waits until Run reports where it listens and returns that
// address. A Run that fails first fails the test.
func listenAddress(t *testing.T, logs *logBuffer, failed <-chan error) string {
	t.Helper()
	var address string
	if !waittest.Poll(20*time.Second, func() bool {
		select {
		case err := <-failed:
			t.Fatalf("the server stopped before it listened: %v", err)
		default:
		}
		if match := servingHTTP.FindStringSubmatch(logs.String()); match != nil {
			address = match[1]
		}
		return address != ""
	}) {
		t.Fatalf("the server never reported its address:\n%s", logs.String())
	}
	return address
}

// selfSigned writes a certificate for 127.0.0.1 and its key.
func selfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "forge test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func get(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client.Do(request)
}

// waitServing retries until the server at url answers.
func waitServing(t *testing.T, client *http.Client, url string, failed <-chan error) *http.Response {
	t.Helper()
	var response *http.Response
	var err error
	if !waittest.Poll(20*time.Second, func() bool {
		select {
		case runErr := <-failed:
			t.Fatalf("the server stopped before it answered: %v", runErr)
		default:
		}
		response, err = get(t, client, url)
		return err == nil
	}) {
		t.Fatalf("the server never answered: %v", err)
	}
	return response
}

func TestNewWithOptionsRejectsMissingPieces(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	cfg := config.Default()
	if _, err := httpserver.NewWithOptions(cfg, nil, quiet, httpserver.Options{}); err == nil {
		t.Error("a nil handler was accepted")
	}
	if _, err := httpserver.NewWithOptions(cfg, ok, nil, httpserver.Options{}); err == nil {
		t.Error("a nil logger was accepted")
	}
	if _, err := httpserver.NewWithOptions(cfg, ok, quiet, httpserver.Options{Middlewares: []httpserver.Middleware{nil}}); err == nil {
		t.Error("a nil middleware was accepted")
	}
	nilMiddleware := func(http.Handler) http.Handler { return nil }
	if _, err := httpserver.NewWithOptions(cfg, ok, quiet, httpserver.Options{Middlewares: []httpserver.Middleware{nilMiddleware}}); err == nil || !strings.Contains(err.Error(), "nil handler") {
		t.Errorf("middleware returning nil: %v", err)
	}
	if _, err := httpserver.NewWithOptions(cfg, ok, quiet, httpserver.Options{Instrument: nilMiddleware}); err == nil || !strings.Contains(err.Error(), "instrumentation") {
		t.Errorf("instrumentation returning nil: %v", err)
	}
	bad := config.Default()
	bad.HTTP.Transport = "carrier-pigeon"
	if _, err := httpserver.NewWithOptions(bad, ok, quiet, httpserver.Options{}); err == nil {
		t.Error("an invalid configuration was accepted")
	}
}

func TestRecovererLetsAnAbortedResponseThrough(t *testing.T) {
	t.Parallel()
	server, err := httpserver.New(config.Default(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }), quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Errorf("recovered = %v, want http.ErrAbortHandler to reach net/http", recovered)
		}
	}()
	server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestRunServesPlainHTTPAndStopsOnCancel(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.HTTP.Address = anyAddress
	logger, logs := newLogs()
	server, err := httpserver.New(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hello")) }), logger)
	if err != nil {
		t.Fatal(err)
	}
	var nilCtx context.Context
	if err := server.Run(nilCtx); err == nil {
		t.Error("a nil context was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Run(ctx) }()
	address := listenAddress(t, logs, result)
	if address == anyAddress {
		t.Errorf("the log reports the configured address %s, not the bound one", address)
	}
	response := waitServing(t, http.DefaultClient, "http://"+address+"/", result)
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "hello" || response.Header.Get("Strict-Transport-Security") != "" {
		t.Errorf("body=%q headers=%v", body, response.Header)
	}
	cancel()
	if err := <-result; err != nil {
		t.Errorf("a graceful stop = %v", err)
	}
}

func TestRunServesTLSWithHSTS(t *testing.T) {
	t.Parallel()
	certFile, keyFile := selfSigned(t)
	logger, logs := newLogs()
	cfg := config.Default()
	cfg.HTTP.Address = anyAddress
	cfg.HTTP.Transport = config.TransportTLS
	cfg.HTTP.TLSCertFile, cfg.HTTP.TLSKeyFile = certFile, keyFile
	server, err := httpserver.New(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secure")) }), logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Run(ctx) }()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}} // #nosec G402 -- a self-signed loopback test certificate
	address := listenAddress(t, logs, result)
	response := waitServing(t, client, "https://"+address+"/", result)
	_ = response.Body.Close()
	if !strings.Contains(response.Header.Get("Strict-Transport-Security"), "max-age=") {
		t.Errorf("headers = %v", response.Header)
	}
	cancel()
	if err := <-result; err != nil {
		t.Errorf("a graceful stop = %v", err)
	}
}

func TestRunReleasesTheListenerWhenTheCertificateCannotBeLoaded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for _, name := range []string{certFile, keyFile} {
		if err := os.WriteFile(name, []byte("not PEM"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logger, logs := newLogs()
	cfg := config.Default()
	cfg.HTTP.Address = anyAddress
	cfg.HTTP.Transport = config.TransportTLS
	cfg.HTTP.TLSCertFile, cfg.HTTP.TLSKeyFile = certFile, keyFile
	server, err := httpserver.New(cfg, http.NotFoundHandler(), logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "serve HTTP") {
		t.Fatalf("Run with an unreadable certificate = %v", err)
	}
	address := listenAddress(t, logs, nil)
	// The bound port is free again: binding it succeeds.
	again, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("the listener of a failed run still holds %s: %v", address, err)
	}
	_ = again.Close()
}

func TestRunReportsAnAddressThatCannotBeServed(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	cfg := config.Default()
	cfg.HTTP.Address = taken.Addr().String()
	server, err := httpserver.New(cfg, http.NotFoundHandler(), quiet)
	if err != nil {
		t.Fatal(err)
	}
	err = server.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "serve HTTP") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunClosesConnectionsWhenTheDrainDeadlinePasses(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.HTTP.Address = anyAddress
	cfg.HTTP.ShutdownTimeout = 50 * time.Millisecond
	logger, logs := newLogs()
	started := make(chan struct{})
	canceled := make(chan struct{})
	server, err := httpserver.New(cfg, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // only closing the connection ends this request
		close(canceled)
	}), logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Run(ctx) }()
	address := listenAddress(t, logs, result)
	go func() {
		// Retry until the request is in the handler.
		for {
			response, err := get(t, &http.Client{}, "http://"+address+"/")
			if err == nil {
				_ = response.Body.Close()
				return
			}
			select {
			case <-started:
				return
			case <-t.Context().Done():
				return // the test ended early; do not retry forever
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("the server stopped before the request: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the request never reached the handler")
	}
	cancel()
	err = <-result
	if err == nil || !strings.Contains(err.Error(), "shutdown HTTP") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the drain deadline", err)
	}
	select {
	case <-canceled:
	case <-time.After(20 * time.Second):
		t.Fatal("the stuck handler's request context was never canceled")
	}
}

// A request still in flight when the context is canceled is answered: the
// graceful drain waits for it, and Run returns nil because the deadline was
// not reached.
func TestRunDrainsAnInFlightRequestBeforeTheDeadline(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.HTTP.Address = anyAddress
	cfg.HTTP.ShutdownTimeout = 20 * time.Second
	logger, logs := newLogs()
	started := make(chan struct{})
	release := make(chan struct{})
	server, err := httpserver.New(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte("finished"))
	}), logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Run(ctx) }()
	address := listenAddress(t, logs, result)
	type reply struct {
		body string
		err  error
	}
	replies := make(chan reply, 1)
	go func() {
		client := &http.Client{}
		for {
			response, err := get(t, client, "http://"+address+"/")
			if err != nil {
				select {
				case <-started:
					replies <- reply{err: err}
					return
				case <-t.Context().Done():
					return
				case <-time.After(5 * time.Millisecond):
					continue
				}
			}
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			replies <- reply{body: string(body), err: readErr}
			return
		}
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("the server stopped before the request: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the request never reached the handler")
	}
	cancel()
	select {
	case err := <-result:
		t.Fatalf("Run returned %v while a request was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Errorf("a drained stop = %v", err)
	}
	if got := <-replies; got.err != nil || got.body != "finished" {
		t.Errorf("the in-flight request got %q, %v", got.body, got.err)
	}
}
