package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// bodyReadTimeout bounds how long a client may take to send a body that the
// proxy buffers before forwarding, and how long any single read of a
// larger, streamed body may wait for data.
const bodyReadTimeout = 30 * time.Second

// Server routes requests to the current target of each service. Create it
// with [New]; it is safe for concurrent use.
type Server struct {
	logger    *slog.Logger
	statePath string
	health    *http.Client
	// bodyTimeout is bodyReadTimeout; tests shorten it.
	bodyTimeout time.Duration

	mu       sync.RWMutex
	services map[string]*service
	hosts    map[string]*service
	fallback *service
	locks    map[string]*sync.Mutex

	stateMu sync.Mutex
}

// service is one routing entry. Its fields are swapped atomically so that
// requests never take a lock on the hot path.
type service struct {
	name        string
	settings    atomic.Pointer[DeployRequest]
	target      atomic.Pointer[target]
	maintenance atomic.Bool
}

// target is one backend container. Requests acquire it before forwarding;
// once retired, acquire fails and the request re-reads the service's
// current target, which was swapped before retirement.
type target struct {
	addr      string
	proxy     *httputil.ReverseProxy
	transport *http.Transport

	mu      sync.Mutex
	retired bool
	active  int64
	idle    chan struct{}
	closed  bool
}

// New creates a server and loads the routing state saved at statePath, if
// any. An empty statePath disables persistence.
func New(logger *slog.Logger, statePath string) (*Server, error) {
	s := &Server{
		logger:      logger,
		statePath:   statePath,
		bodyTimeout: bodyReadTimeout,
		services:    make(map[string]*service),
		hosts:       make(map[string]*service),
		locks:       make(map[string]*sync.Mutex),
		health: &http.Client{
			Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	if statePath != "" {
		if err := s.loadState(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func newTarget(addr string, settings *DeployRequest, logger *slog.Logger) *target {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: time.Duration(settings.ResponseTimeout),
		ExpectContinueTimeout: time.Second,
	}
	backend := &url.URL{Scheme: "http", Host: addr}
	t := &target{addr: addr, transport: transport, idle: make(chan struct{})}
	t.proxy = &httputil.ReverseProxy{
		// Rewrite (unlike Director) drops inbound Forwarded and
		// X-Forwarded-* headers, so clients cannot spoof them.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(backend)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: transport,
		ErrorLog:  slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if maxBytes := (*http.MaxBytesError)(nil); errors.As(err, &maxBytes) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			if bodyTimedOut(r) {
				// A streamed body stalled past the per-read deadline; the
				// server cancels the request context too, so check first.
				http.Error(w, "request body not received in time", http.StatusRequestTimeout)
				return
			}
			if r.Context().Err() != nil {
				// The client went away; nobody reads the response.
				w.WriteHeader(499)
				return
			}
			logger.Warn("upstream error", "target", addr, "error", err.Error())
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
	return t
}

func (t *target) acquire() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.retired {
		return false
	}
	t.active++
	return true
}

func (t *target) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active--
	if t.retired && t.active == 0 && !t.closed {
		close(t.idle)
		t.closed = true
	}
}

// retire stops new requests from acquiring t and returns a channel closed
// once the requests in flight have finished.
func (t *target) retire() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.retired {
		t.retired = true
		if t.active == 0 {
			close(t.idle)
			t.closed = true
		}
	}
	return t.idle
}

func (t *target) inFlight() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active
}

// drain retires t and waits up to timeout for its requests to finish. It
// reports whether they did and how many remain.
func (t *target) drain(timeout time.Duration) (bool, int64) {
	idle := t.retire()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		t.transport.CloseIdleConnections()
		return true, 0
	case <-timer.C:
		t.transport.CloseIdleConnections()
		return false, t.inFlight()
	}
}

// ServeHTTP routes r by its Host header.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	recorder := &statusRecorder{ResponseWriter: w}
	svc := s.route(r.Host)
	targetAddr := s.serve(recorder, r, svc)
	name := ""
	if svc != nil {
		name = svc.name
	}
	status := recorder.status
	if status == 0 {
		status = http.StatusOK
	}
	clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		clientIP = r.RemoteAddr
	}
	// The query string is not logged: it can carry tokens.
	s.logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
		slog.String("service", name),
		slog.String("host", r.Host),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Int64("bytes", recorder.bytes),
		slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		slog.String("client_ip", clientIP),
		slog.String("proto", r.Proto),
		slog.Bool("tls", r.TLS != nil),
		slog.String("target", targetAddr),
	)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, svc *service) string {
	if svc == nil {
		http.Error(w, "unknown host", http.StatusNotFound)
		return ""
	}
	if svc.maintenance.Load() {
		writeMaintenance(w)
		return ""
	}
	settings := svc.settings.Load()
	if settings.TLS && r.TLS == nil {
		redirect := url.URL{Scheme: "https", Host: stripPort(r.Host), Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, redirect.String(), http.StatusPermanentRedirect)
		return ""
	}
	if r.ContentLength > settings.MaxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return ""
	}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, settings.MaxBodyBytes)
		if !s.prepareBody(w, r, settings.BufferBytes) {
			return ""
		}
	}
	for {
		t := svc.target.Load()
		if t == nil {
			http.Error(w, "service has no target", http.StatusServiceUnavailable)
			return ""
		}
		if t.acquire() {
			defer t.release()
			t.proxy.ServeHTTP(w, r)
			return t.addr
		}
		if svc.target.Load() == t {
			// Retired without a successor: the service was removed
			// after this request was routed.
			http.Error(w, "service has no target", http.StatusServiceUnavailable)
			return ""
		}
		// t was retired after we loaded it; the current target has
		// already been swapped in, so the next load returns it.
	}
}

// prepareBody protects the application from slow clients. Bodies of at
// most limit bytes, with or without Content-Length, are read completely
// within the body timeout before the application is contacted (408
// otherwise). Larger bodies are streamed, and every read must receive
// data within the body timeout; a stalled upload gets 408.
func (s *Server) prepareBody(w http.ResponseWriter, r *http.Request, limit int64) bool {
	controller := http.NewResponseController(w)
	if r.ContentLength > limit {
		r.Body = &deadlineBody{body: r.Body, controller: controller, timeout: s.bodyTimeout}
		return true
	}
	_ = controller.SetReadDeadline(time.Now().Add(s.bodyTimeout))
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		if maxBytes := (*http.MaxBytesError)(nil); errors.As(err, &maxBytes) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "request body not received in time", http.StatusRequestTimeout)
		}
		return false
	}
	if int64(len(body)) > limit {
		// Unknown length beyond the buffer: stream the rest.
		rest := &deadlineBody{body: r.Body, controller: controller, timeout: s.bodyTimeout}
		r.Body = &prefixedBody{reader: io.MultiReader(bytes.NewReader(body), rest), rest: rest}
		return true
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	return true
}

// deadlineBody gives every read of a streamed body its own deadline and
// clears it when the body ends, so the connection's later reads are not
// affected.
type deadlineBody struct {
	body       io.ReadCloser
	controller *http.ResponseController
	timeout    time.Duration
	expired    atomic.Bool
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	_ = b.controller.SetReadDeadline(time.Now().Add(b.timeout))
	n, err := b.body.Read(p)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			b.expired.Store(true)
		}
		_ = b.controller.SetReadDeadline(time.Time{})
	}
	return n, err
}

// prefixedBody is a buffered prefix followed by the streamed rest.
type prefixedBody struct {
	reader io.Reader
	rest   *deadlineBody
}

func (b *prefixedBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *prefixedBody) Close() error               { return b.rest.Close() }

// bodyTimedOut reports whether r's streamed body hit its read deadline.
func bodyTimedOut(r *http.Request) bool {
	switch body := r.Body.(type) {
	case *deadlineBody:
		return body.expired.Load()
	case *prefixedBody:
		return body.rest.expired.Load()
	}
	return false
}

func (b *deadlineBody) Close() error {
	_ = b.controller.SetReadDeadline(time.Time{})
	return b.body.Close()
}

const maintenancePage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Down for maintenance</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem;color:#222}</style>
</head><body><h1>Down for maintenance</h1><p>We are performing scheduled maintenance and will be back shortly.</p></body></html>
`

func writeMaintenance(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "60")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, maintenancePage)
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func (s *Server) route(host string) *service {
	host = strings.TrimSuffix(strings.ToLower(stripPort(host)), ".")
	s.mu.RLock()
	defer s.mu.RUnlock()
	if svc, ok := s.hosts[host]; ok {
		return svc
	}
	return s.fallback
}

// TLSHost reports whether host belongs to a service with TLS enabled; the
// ACME manager issues certificates only for those hosts.
func (s *Server) TLSHost(host string) bool {
	host = strings.ToLower(host)
	s.mu.RLock()
	defer s.mu.RUnlock()
	svc, ok := s.hosts[host]
	return ok && svc.settings.Load().TLS
}

func (s *Server) serviceLock(name string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, ok := s.locks[name]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[name] = lock
	}
	return lock
}

// ErrUnhealthy wraps health-check failures returned by [Server.Deploy].
var ErrUnhealthy = errors.New("target failed its health check")

// Deploy registers req.Target, waits until it passes its health check
// req.HealthyCount times in a row, switches the service to it atomically
// and drains the previous target for up to req.DrainTimeout. If the health
// check does not pass within req.DeployTimeout, the previous target keeps
// serving and an error wrapping [ErrUnhealthy] is returned.
func (s *Server) Deploy(ctx context.Context, req DeployRequest) (DeployResult, error) {
	if err := req.normalize(); err != nil {
		return DeployResult{}, err
	}
	lock := s.serviceLock(req.Service)
	lock.Lock()
	if err := s.checkHosts(req.Service, req.Hosts); err != nil {
		lock.Unlock()
		return DeployResult{}, err
	}
	next := newTarget(req.Target, &req, s.logger)
	checks, err := s.waitHealthy(ctx, &req)
	if err != nil {
		lock.Unlock()
		next.transport.CloseIdleConnections()
		s.logger.Warn("deploy rejected", "service", req.Service, "target", req.Target, "error", err.Error())
		return DeployResult{}, err
	}
	s.mu.Lock()
	if err := s.checkHostsLocked(req.Service, req.Hosts); err != nil {
		s.mu.Unlock()
		lock.Unlock()
		return DeployResult{}, err
	}
	svc, ok := s.services[req.Service]
	if !ok {
		svc = &service{name: req.Service}
		s.services[req.Service] = svc
	}
	settings := req
	svc.settings.Store(&settings)
	previous := svc.target.Swap(next)
	s.reindexLocked()
	s.mu.Unlock()
	result := DeployResult{Service: req.Service, Target: req.Target, Drained: true, HealthChecks: checks}
	if err := s.saveState(); err != nil {
		result.StateSaveFailed = err.Error()
		s.logger.Error("saving proxy state failed", "error", err.Error())
	}
	lock.Unlock()
	s.logger.Info("target switched", "service", req.Service, "target", req.Target)
	if previous != nil {
		result.Previous = previous.addr
		result.Drained, result.InFlight = previous.drain(time.Duration(req.DrainTimeout))
		s.logger.Info("previous target drained", "service", req.Service, "target", previous.addr, "drained", result.Drained, "in_flight", result.InFlight)
	}
	return result, nil
}

func (s *Server) checkHosts(name string, hosts []string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checkHostsLocked(name, hosts)
}

func (s *Server) checkHostsLocked(name string, hosts []string) error {
	for _, host := range hosts {
		if owner, ok := s.hosts[host]; ok && owner.name != name {
			return fmt.Errorf("host %s is already routed to service %s", host, owner.name)
		}
	}
	if len(hosts) == 0 && s.fallback != nil && s.fallback.name != name {
		return fmt.Errorf("service %s already receives requests for unmatched hosts", s.fallback.name)
	}
	return nil
}

func (s *Server) reindexLocked() {
	s.hosts = make(map[string]*service)
	s.fallback = nil
	for _, svc := range s.services {
		settings := svc.settings.Load()
		if len(settings.Hosts) == 0 {
			s.fallback = svc
		}
		for _, host := range settings.Hosts {
			s.hosts[host] = svc
		}
	}
}

func (s *Server) waitHealthy(ctx context.Context, req *DeployRequest) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.DeployTimeout))
	defer cancel()
	consecutive, checks := 0, 0
	last := "no check completed"
	for {
		checks++
		if err := s.probe(ctx, req); err != nil {
			consecutive = 0
			// A probe cut short by the deploy deadline says nothing new;
			// keep the previous failure for the error message.
			if ctx.Err() == nil || last == "no check completed" {
				last = err.Error()
			}
		} else {
			consecutive++
			if consecutive >= req.HealthyCount {
				return checks, nil
			}
		}
		timer := time.NewTimer(time.Duration(req.HealthInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return checks, fmt.Errorf("%w: %s not healthy within %s (%d checks, last: %s)", ErrUnhealthy, req.Target, time.Duration(req.DeployTimeout), checks, last)
		case <-timer.C:
		}
	}
}

func (s *Server) probe(ctx context.Context, req *DeployRequest) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.HealthTimeout))
	defer cancel()
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+req.Target+req.HealthPath, nil)
	if err != nil {
		return err
	}
	if len(req.Hosts) > 0 {
		probe.Host = req.Hosts[0]
	}
	resp, err := s.health.Do(probe)
	if err != nil {
		return errors.New("request failed")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// SetMaintenance turns the maintenance page on or off for a service.
func (s *Server) SetMaintenance(name string, on bool) error {
	s.mu.RLock()
	svc, ok := s.services[name]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown service %q", name)
	}
	svc.maintenance.Store(on)
	return s.saveState()
}

// Remove unregisters a service and drains its target.
func (s *Server) Remove(name string, drainTimeout time.Duration) error {
	lock := s.serviceLock(name)
	lock.Lock()
	s.mu.Lock()
	svc, ok := s.services[name]
	if !ok {
		s.mu.Unlock()
		lock.Unlock()
		return fmt.Errorf("unknown service %q", name)
	}
	delete(s.services, name)
	s.reindexLocked()
	// Requests routed before the removal see no target (503) instead of
	// a retired one.
	previous := svc.target.Swap(nil)
	s.mu.Unlock()
	err := s.saveState()
	lock.Unlock()
	if previous != nil {
		previous.drain(drainTimeout)
	}
	return err
}

// ServiceStatus describes one service for `forge-proxy status`.
type ServiceStatus struct {
	Service     string   `json:"service"`
	Hosts       []string `json:"hosts"`
	TLS         bool     `json:"tls"`
	Target      string   `json:"target"`
	Maintenance bool     `json:"maintenance"`
	InFlight    int64    `json:"in_flight"`
	HealthPath  string   `json:"health_path"`
}

// Status lists the services sorted by name.
func (s *Server) Status() []ServiceStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statusLocked()
}

func (s *Server) statusLocked() []ServiceStatus {
	statuses := make([]ServiceStatus, 0, len(s.services))
	for _, svc := range s.services {
		settings := svc.settings.Load()
		status := ServiceStatus{
			Service: svc.name, Hosts: append([]string{}, settings.Hosts...), TLS: settings.TLS,
			Maintenance: svc.maintenance.Load(), HealthPath: settings.HealthPath,
		}
		if t := svc.target.Load(); t != nil {
			status.Target = t.addr
			status.InFlight = t.inFlight()
		}
		statuses = append(statuses, status)
	}
	sortStatuses(statuses)
	return statuses
}

func sortStatuses(statuses []ServiceStatus) {
	slices.SortFunc(statuses, func(a, b ServiceStatus) int { return strings.Compare(a.Service, b.Service) })
}

// statusRecorder captures the status and size for the access log. Unwrap
// lets http.ResponseController reach Flush and Hijack on the original
// writer, which the reverse proxy needs for streaming and upgrades.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
