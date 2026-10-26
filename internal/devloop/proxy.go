package devloop

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// ReservedPrefix is the path prefix the proxy answers itself; requests
	// below it never reach the application.
	ReservedPrefix = "/__forge/"
	scriptPath     = ReservedPrefix + "reload.js"
	eventsPath     = ReservedPrefix + "events"

	// DefaultMaxInjectBytes bounds the HTML document the proxy buffers to
	// inject the script; larger responses pass through untouched.
	DefaultMaxInjectBytes = 4 << 20

	keepAliveInterval = 20 * time.Second
	shutdownTimeout   = 5 * time.Second
	dialTimeout       = 2 * time.Second
)

//go:embed reload.js
var reloadScript []byte

// ProxyOptions configures a Proxy.
type ProxyOptions struct {
	// Environment is the resolved FORGE_ENV. NewProxy refuses anything but
	// "development": the reload machinery exists only there.
	Environment string
	// ShowBuildErrors sends compiler output to browsers. Set it only when
	// the proxy listens on a loopback address; requests whose Host is not a
	// loopback name never receive it either way.
	ShowBuildErrors bool
	// MaxInjectBytes defaults to DefaultMaxInjectBytes.
	MaxInjectBytes int64
	// Logf reports proxy errors to the developer; nil discards.
	Logf func(format string, args ...any)
}

// Proxy fronts the development application. It forwards requests to the
// application's current address, injects the reload script into HTML
// responses, serves the script and the event stream itself, and tells
// connected browsers to reload when the application restarted.
type Proxy struct {
	opts      ProxyOptions
	hub       *hub
	bootID    string
	transport *http.Transport
	rp        *httputil.ReverseProxy

	mu       sync.Mutex
	upstream *url.URL
	status   string // shown when there is no upstream
	buildErr string // last failed build's output; "" when the build is good
}

// NewProxy returns a proxy; it fails unless opts.Environment is development.
func NewProxy(opts ProxyOptions) (*Proxy, error) {
	if opts.Environment != "development" {
		return nil, fmt.Errorf("devloop: browser reload is development-only, FORGE_ENV is %q", opts.Environment)
	}
	if opts.MaxInjectBytes <= 0 {
		opts.MaxInjectBytes = DefaultMaxInjectBytes
	}
	boot, err := randomToken()
	if err != nil {
		return nil, err
	}
	p := &Proxy{opts: opts, hub: newHub(), bootID: boot, status: "Starting the application"}
	p.transport = &http.Transport{
		DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
		MaxIdleConns:        8,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
		TLSHandshakeTimeout: dialTimeout,
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		Transport:      p.transport,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.upstreamError,
		FlushInterval:  -1, // stream, so the application's own SSE and chunked output work
		ErrorLog:       log.New(io.Discard, "", 0),
	}
	return p, nil
}

type upstreamKey struct{}

// SetUpstream points the proxy at the application's address (host:port) and
// clears any build error. It does not reload browsers; call Reload.
func (p *Proxy) SetUpstream(addr string) {
	p.mu.Lock()
	p.upstream = &url.URL{Scheme: "http", Host: addr}
	p.buildErr = ""
	p.mu.Unlock()
	p.transport.CloseIdleConnections()
}

// Restarting shows the wait page until SetUpstream is called again.
func (p *Proxy) Restarting() { p.setStatus("Restarting the application") }

// Unavailable shows a page explaining why there is no application to serve.
func (p *Proxy) Unavailable(reason string) { p.setStatus(reason) }

func (p *Proxy) setStatus(status string) {
	p.mu.Lock()
	p.upstream = nil
	p.status = status
	p.mu.Unlock()
	p.transport.CloseIdleConnections()
}

// Reload tells every connected browser to reload its page.
func (p *Proxy) Reload() { p.hub.broadcast(event{name: "reload", data: "1"}) }

// BuildFailed records the compiler output and shows it in connected browsers.
// The application, if one runs, keeps being served.
func (p *Proxy) BuildFailed(output string) {
	p.mu.Lock()
	p.buildErr = output
	p.mu.Unlock()
	p.hub.broadcast(buildErrorEvent(output, p.opts.ShowBuildErrors))
}

func buildErrorEvent(output string, show bool) event {
	message := ""
	if show {
		message = output
	}
	data, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{message})
	return event{name: "build-error", data: string(data)}
}

// Close ends every open event stream.
func (p *Proxy) Close() { p.hub.close() }

// Handler returns the proxy's http.Handler. It routes on the exact request
// path rather than through http.ServeMux, which would redirect unclean paths
// instead of forwarding them as the browser sent them.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == scriptPath && r.Method == http.MethodGet:
			p.serveScript(w, r)
		case r.URL.Path == eventsPath && r.Method == http.MethodGet:
			p.serveEvents(w, r)
		case strings.HasPrefix(r.URL.Path, ReservedPrefix):
			http.NotFound(w, r)
		default:
			p.forward(w, r)
		}
	})
}

// Serve serves on l until ctx ends, then closes the event streams and
// shuts the server down. One goroutine runs, and Serve waits for it.
func (p *Proxy) Serve(ctx context.Context, l net.Listener) error {
	server := &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- server.Serve(l) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	p.hub.close()
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close()
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (p *Proxy) serveScript(w http.ResponseWriter, _ *http.Request) {
	header := w.Header()
	header.Set("Content-Type", "text/javascript; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(reloadScript)
}

func (p *Proxy) serveEvents(w http.ResponseWriter, r *http.Request) {
	events, unsubscribe, ok := p.hub.subscribe()
	if !ok {
		http.Error(w, "too many open reload streams", http.StatusServiceUnavailable)
		return
	}
	defer unsubscribe()
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	write := func(e event) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !write(event{name: "hello", data: p.bootID}) {
		return
	}
	p.mu.Lock()
	failed := p.buildErr
	p.mu.Unlock()
	reveal := p.mayShowBuildErrors(r)
	if failed != "" && !write(buildErrorEvent(failed, reveal)) {
		return
	}
	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if e.name == "build-error" && !reveal {
				e = buildErrorEvent("", false)
			}
			if !write(e) {
				return
			}
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil || controller.Flush() != nil {
				return
			}
		}
	}
}

// loopbackHost reports whether the Host header names this machine, which
// keeps a DNS-rebound page from reading compiler output.
func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = strings.Trim(hostport, "[]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	target := p.upstream
	status := p.status
	p.mu.Unlock()
	if target == nil {
		p.statusPage(w, http.StatusServiceUnavailable, status, p.mayShowBuildErrors(r))
		return
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), upstreamKey{}, target)))
}

func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	target, _ := pr.In.Context().Value(upstreamKey{}).(*url.URL)
	pr.SetURL(target)
	pr.Out.Host = pr.In.Host // the application sees the Host the browser used
	pr.SetXForwarded()
	// Responses are rewritten, so ask for them uncompressed.
	pr.Out.Header.Set("Accept-Encoding", "identity")
}

func (p *Proxy) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if p.opts.Logf != nil && !errors.Is(err, context.Canceled) {
		p.opts.Logf("proxy: %v", err)
	}
	p.statusPage(w, http.StatusBadGateway, "The application is not answering", p.mayShowBuildErrors(r))
}

// mayShowBuildErrors reports whether compiler output may be shown to the
// requester: the proxy listens on loopback and the request names a loopback
// Host, which a DNS-rebound page cannot fake.
func (p *Proxy) mayShowBuildErrors(r *http.Request) bool {
	return p.opts.ShowBuildErrors && loopbackHost(r.Host)
}

// statusPage is the page shown while there is no application to proxy to. It
// connects to the event stream, so it reloads itself when the application
// is back.
func (p *Proxy) statusPage(w http.ResponseWriter, code int, status string, reveal bool) {
	nonce, err := randomToken()
	if err != nil {
		http.Error(w, status, code)
		return
	}
	p.mu.Lock()
	failed := p.buildErr
	p.mu.Unlock()
	detail := ""
	if failed != "" && reveal {
		detail = "<pre>" + html.EscapeString(failed) + "</pre>"
	}
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("Retry-After", "1")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; style-src 'nonce-"+nonce+"'; script-src 'nonce-"+nonce+"'; connect-src 'self'; frame-ancestors 'none'")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>forge dev</title>`+
		`<style nonce="%[1]s">body{font:16px/1.5 system-ui,sans-serif;margin:3rem auto;max-width:48rem;padding:0 1rem}pre{background:#fef2f2;padding:1rem;overflow:auto;white-space:pre-wrap}</style></head>`+
		`<body><h1>forge dev</h1><p>%[2]s.</p>%[3]s<script src="%[4]s" nonce="%[1]s"></script></body></html>`,
		nonce, html.EscapeString(status), detail, scriptPath)
}

// modifyResponse injects the reload script into HTML pages and relaxes the
// page's Content-Security-Policy just enough for it. A response of known
// length is buffered (up to MaxInjectBytes) so Content-Length stays right; a
// chunked or streamed one is passed on as it arrives, holding back only the
// last few kilobytes in which the closing tag lives.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	if !injectable(resp) {
		return nil
	}
	nonce, err := randomToken()
	if err != nil {
		return err
	}
	if resp.ContentLength < 0 {
		resp.Header.Del("Content-Length")
		p.relax(resp, nonce)
		resp.Body = &injectingBody{src: resp.Body, nonce: nonce}
		return nil
	}
	limit := p.opts.MaxInjectBytes
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		// Too large to buffer: serve it as the application sent it.
		resp.Body = &joinedBody{Reader: io.MultiReader(bytes.NewReader(body), resp.Body), closer: resp.Body}
		return nil
	}
	_ = resp.Body.Close()
	injected, ok := injectScript(body, nonce)
	if !ok {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(injected))
	resp.ContentLength = int64(len(injected))
	resp.Header.Set("Content-Length", strconv.Itoa(len(injected)))
	p.relax(resp, nonce)
	return nil
}

// relax makes an injected response uncacheable and lets the script run under
// the page's policies.
func (p *Proxy) relax(resp *http.Response, nonce string) {
	resp.Header.Set("Cache-Control", "no-store")
	resp.Header.Del("ETag")
	resp.Header.Del("Last-Modified")
	if values := resp.Header.Values("Content-Security-Policy"); len(values) > 0 {
		relaxed := make([]string, len(values))
		for i, value := range values {
			relaxed[i] = allowReload(value, nonce)
		}
		resp.Header["Content-Security-Policy"] = relaxed
	}
}

// injectingBody streams an HTML body, holding back its last injectionWindow
// bytes; at the end it adds the script to that tail if it holds the closing
// body tag.
type injectingBody struct {
	src   io.ReadCloser
	nonce string
	hold  []byte // the most recent bytes, at most injectionWindow after a read
	out   []byte // bytes ready to be handed to the reader
	done  bool
	err   error
	chunk [32 << 10]byte
}

func (b *injectingBody) Read(p []byte) (int, error) {
	for len(b.out) == 0 && !b.done {
		n, err := b.src.Read(b.chunk[:])
		b.hold = append(b.hold, b.chunk[:n]...)
		if extra := len(b.hold) - injectionWindow; extra > 0 {
			b.out = append(b.out, b.hold[:extra]...)
			b.hold = append(b.hold[:0], b.hold[extra:]...)
		}
		if err != nil {
			tail := b.hold
			if injected, ok := injectScript(tail, b.nonce); ok {
				tail = injected
			}
			b.out = append(b.out, tail...)
			b.hold, b.done, b.err = nil, true, err
		}
	}
	if len(b.out) > 0 {
		n := copy(p, b.out)
		b.out = b.out[n:]
		return n, nil
	}
	return 0, b.err
}

func (b *injectingBody) Close() error { return b.src.Close() }

type joinedBody struct {
	io.Reader
	closer io.Closer
}

func (b *joinedBody) Close() error { return b.closer.Close() }

// injectable reports whether the response is an HTML page the script can be
// added to: a body-carrying, uncompressed, inline text/html response.
func injectable(resp *http.Response) bool {
	if resp.Request != nil && resp.Request.Method == http.MethodHead {
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return false
	}
	if resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Range") != "" {
		return false
	}
	if disposition := resp.Header.Get("Content-Disposition"); strings.HasPrefix(strings.ToLower(disposition), "attachment") {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && mediaType == "text/html"
}

const injectionWindow = 4096

// injectScript adds the script before the closing body tag. HTML fragments
// (no </body>) are left alone: they are partials, not pages.
func injectScript(body []byte, nonce string) ([]byte, bool) {
	if bytes.Contains(body, []byte(scriptPath)) {
		return nil, false
	}
	start := max(0, len(body)-injectionWindow)
	index := lastIndexFoldASCII(body[start:], "</body")
	if index < 0 {
		return nil, false
	}
	index += start
	tag := `<script src="` + scriptPath + `" nonce="` + nonce + `"></script>`
	out := make([]byte, 0, len(body)+len(tag))
	out = append(out, body[:index]...)
	out = append(out, tag...)
	out = append(out, body[index:]...)
	return out, true
}

// lastIndexFoldASCII finds the last occurrence of the lowercase ASCII needle
// in b, ignoring ASCII case, on the original bytes. bytes.ToLower is not an
// option: it rewrites invalid UTF-8 (Latin-1 pages) and some runes into
// sequences of another length, which moves every offset.
func lastIndexFoldASCII(b []byte, needle string) int {
	for i := len(b) - len(needle); i >= 0; i-- {
		match := true
		for j := 0; j < len(needle); j++ {
			c := b[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func randomToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("devloop: random token: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(raw[:]), nil
}
