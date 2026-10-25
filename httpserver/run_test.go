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
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/httpserver"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// loopbackAddress returns a free loopback address.
func loopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
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
	deadline := time.Now().Add(20 * time.Second)
	for {
		response, err := get(t, client, url)
		if err == nil {
			return response
		}
		select {
		case runErr := <-failed:
			t.Fatalf("the server stopped before it answered: %v", runErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never answered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	cfg.HTTP.Address = loopbackAddress(t)
	server, err := httpserver.New(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hello")) }), quiet)
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
	response := waitServing(t, http.DefaultClient, "http://"+cfg.HTTP.Address+"/", result)
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
	cfg := config.Default()
	cfg.HTTP.Address = loopbackAddress(t)
	cfg.HTTP.Transport = config.TransportTLS
	cfg.HTTP.TLSCertFile, cfg.HTTP.TLSKeyFile = certFile, keyFile
	server, err := httpserver.New(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secure")) }), quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Run(ctx) }()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}} // #nosec G402 -- a self-signed loopback test certificate
	response := waitServing(t, client, "https://"+cfg.HTTP.Address+"/", result)
	_ = response.Body.Close()
	if !strings.Contains(response.Header.Get("Strict-Transport-Security"), "max-age=") {
		t.Errorf("headers = %v", response.Header)
	}
	cancel()
	if err := <-result; err != nil {
		t.Errorf("a graceful stop = %v", err)
	}
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
	cfg.HTTP.Address = loopbackAddress(t)
	cfg.HTTP.ShutdownTimeout = 50 * time.Millisecond
	started := make(chan struct{})
	canceled := make(chan struct{})
	server, err := httpserver.New(cfg, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // only closing the connection ends this request
		close(canceled)
	}), quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Run(ctx) }()
	go func() {
		// The server may not be listening yet: retry refused connections
		// until the request is in the handler.
		for {
			response, err := get(t, &http.Client{}, "http://"+cfg.HTTP.Address+"/")
			if err == nil {
				_ = response.Body.Close()
				return
			}
			select {
			case <-started:
				return
			default:
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
