// Package forge composes the framework runtime.
package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/health"
	"github.com/rocketinsidecorp/forge-go/httpserver"
	"github.com/rocketinsidecorp/forge-go/openapi"
	"github.com/rocketinsidecorp/forge-go/router"
	"github.com/rocketinsidecorp/forge-go/telemetry"
	htmlviews "github.com/rocketinsidecorp/forge-go/views"
	"github.com/rocketinsidecorp/forge-go/web"
)

const liveRoutePath = "/health/live"

// OpenAPIPath is where the application serves its OpenAPI document when
// FORGE_OPENAPI_ENABLED allows it (config.Config.ServesOpenAPI). The route is
// mounted when the first request arrives, and not at all when the
// application registered a route for this path itself.
const OpenAPIPath = "/openapi.json"

// ViewRenderer executes server-side HTML templates with contextual escaping.
// It is an alias for views.Renderer, exposed here for generated apps that
// vendor the main Forge package.
type ViewRenderer = htmlviews.Renderer

// NewViewRenderer parses *.gohtml templates from templates.
func NewViewRenderer(templates fs.FS) (*ViewRenderer, error) {
	return htmlviews.New(templates)
}

// Option configures an App built by New.
type Option func(*options) error

type options struct {
	config          config.Config
	logger          *slog.Logger
	httpMiddlewares []httpserver.Middleware
	telemetry       *telemetry.Telemetry
	telemetrySet    bool
}

// App is a Forge application: the router, the health registry, the HTTP server with its fixed
// security chain, telemetry and the shutdown hooks. Register routes, middleware and readiness
// checks on it before it serves its first request, then call Run.
type App struct {
	config config.Config
	logger *slog.Logger
	router *router.Router
	health *health.Registry
	server *httpserver.Server
	// telemetry is nil when disabled.
	telemetry *telemetry.Telemetry

	hooksMu       sync.Mutex
	shutdownHooks []ShutdownHook
	closed        bool

	// middlewareMu guards middlewares and serving's transition to true;
	// chain is the composed handler the server dispatches to, with the
	// request rewrites of its middleware.
	middlewareMu sync.Mutex
	middlewares  []httpserver.Middleware
	chain        atomic.Pointer[pipeline]
	serving      atomic.Bool

	// apiMu guards the OpenAPI info and the document served at
	// OpenAPIPath, built once from the frozen routes.
	apiMu      sync.Mutex
	apiInfo    openapi.Info
	apiServed  *openapi.Document
	apiHandler http.Handler
}

// pipeline is the handler built from the middleware added with Use and the
// routes, and the RewriteRequest methods of the httpserver.Rewriter handlers
// those middleware returned, in registration order.
type pipeline struct {
	handler  http.Handler
	rewrites []httpserver.Rewriter
}

// ShutdownHook releases a resource, such as a database pool, after the HTTP
// server has drained. ctx is bounded by the configured shutdown timeout.
type ShutdownHook func(ctx context.Context) error

// New builds an App from the options, which default to config.Default and a discarding logger. It
// validates the configuration, registers the built-in health routes (GET /health, /health/live and
// /health/ready) and constructs the HTTP server without listening; Run starts it.
func New(opts ...Option) (*App, error) {
	settings := options{
		config: config.Default(),
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	for _, option := range opts {
		if option == nil {
			return nil, errors.New("nil Forge option")
		}
		if err := option(&settings); err != nil {
			return nil, err
		}
	}
	if err := settings.config.Validate(); err != nil {
		return nil, err
	}

	app := &App{
		config:    settings.config,
		logger:    settings.logger,
		router:    router.New(),
		health:    health.New(),
		telemetry: settings.telemetry,
		apiInfo:   openapi.Info{Title: "Forge application", Version: "0.0.0"},
	}
	if !settings.telemetrySet {
		built, err := telemetry.FromConfig(settings.config, settings.logger)
		if err != nil {
			return nil, err
		}
		if built != nil {
			// The first hook runs last, so spans of the other hooks'
			// work are still exported.
			app.telemetry = built
			app.shutdownHooks = append(app.shutdownHooks, built.Shutdown)
			// With the OTLP log exporter every record also reaches the
			// collector. The export-failure logger above stays the plain
			// one, so a failing export cannot log into its own queue.
			if exported := built.LogHandler(); exported != nil {
				app.logger = slog.New(telemetry.NewTeeHandler(settings.logger.Handler(), exported))
			}
		}
	}
	if err := app.build(settings); err != nil {
		if closeErr := app.Close(context.Background()); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, err
	}
	return app, nil
}

func (a *App) build(settings options) error {
	if err := a.registerHealthRoutes(); err != nil {
		return err
	}
	a.chain.Store(&pipeline{handler: a.router})
	instrument, err := a.telemetry.ServerMiddleware()
	if err != nil {
		return err
	}
	server, err := httpserver.NewWithOptions(a.config, http.HandlerFunc(a.dispatch), a.logger, httpserver.Options{
		Instrument:  instrument,
		Middlewares: settings.httpMiddlewares,
		Rewrite:     a.rewrite,
	})
	if err != nil {
		return err
	}
	a.server = server
	return nil
}

// WithConfig sets the configuration, for example one from config.Load. New validates it.
func WithConfig(cfg config.Config) Option {
	return func(settings *options) error {
		settings.config = cfg
		return nil
	}
}

// WithLogger sets the application logger; it must not be nil.
func WithLogger(logger *slog.Logger) Option {
	return func(settings *options) error {
		if logger == nil {
			return errors.New("logger is required")
		}
		settings.logger = logger
		return nil
	}
}

// WithTelemetry makes the application use t instead of building one from
// the FORGE_TELEMETRY_* configuration; nil disables telemetry. The caller
// owns t and must shut it down. Without this option, New builds the
// Telemetry from configuration and shuts it down with the application.
func WithTelemetry(t *telemetry.Telemetry) Option {
	return func(settings *options) error {
		settings.telemetry = t
		settings.telemetrySet = true
		return nil
	}
}

// WithHTTPMiddleware adds middleware to the application's HTTP handler chain.
// Request rewrites of middleware added here or with App.Use (see
// httpserver.Rewriter, such as the locale path prefix of
// i18n.Translator.Middleware) are applied before the first of these
// middleware runs, so they see the path the router uses.
// Options are applied in declaration order, outermost first among custom
// middleware. Forge's request ID and security headers remain outermost; body
// limits and panic recovery still protect the custom middleware chain.
func WithHTTPMiddleware(middleware httpserver.Middleware) Option {
	return func(settings *options) error {
		if middleware == nil {
			return errors.New("HTTP middleware is required")
		}
		settings.httpMiddlewares = append(settings.httpMiddlewares, middleware)
		return nil
	}
}

// Config returns the validated configuration the application was built with.
func (a *App) Config() config.Config { return a.config }

// Logger returns the application logger.
func (a *App) Logger() *slog.Logger { return a.logger }

// Telemetry returns the application's telemetry, or nil when it is
// disabled. Pass it to the database adapters, queue workers, outbox
// dispatchers, schedulers and mail senders so they are instrumented; every
// Forge component accepts nil.
func (a *App) Telemetry() *telemetry.Telemetry { return a.telemetry }

// OnShutdown registers a hook that runs after the HTTP server stops, in
// reverse registration order, like deferred calls.
func (a *App) OnShutdown(hook ShutdownHook) error {
	if hook == nil {
		return errors.New("shutdown hook is required")
	}
	a.hooksMu.Lock()
	defer a.hooksMu.Unlock()
	if a.closed {
		return errors.New("application already shut down")
	}
	a.shutdownHooks = append(a.shutdownHooks, hook)
	return nil
}

// Close runs the shutdown hooks once. Run calls it; call it directly only when
// the application is discarded without running.
func (a *App) Close(ctx context.Context) error {
	a.hooksMu.Lock()
	if a.closed {
		a.hooksMu.Unlock()
		return nil
	}
	a.closed = true
	hooks := slices.Clone(a.shutdownHooks)
	a.hooksMu.Unlock()

	var errs []error
	for _, hook := range slices.Backward(hooks) {
		if err := hook(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("shutdown hooks: %w", err)
	}
	return nil
}

// Use adds middleware that runs before routing, inside the middleware given
// to WithHTTPMiddleware, in the order Use is called (the first is
// outermost). Configure uses it to install request-wide behavior that must
// see the request before a route is chosen, such as web.MethodOverride,
// csrf.Protection.Handler and web.Flash. The chain is rebuilt on every call,
// so a middleware constructor may run more than once. A middleware whose
// handler is an httpserver.Rewriter (i18n.Translator.Middleware with
// PathPrefix) has its request rewrite applied before every middleware,
// including those given to WithHTTPMiddleware and those added with Use
// before it, so a path guard anywhere in the chain sees the path the router
// uses. After the first request has reached the application middleware, Use
// returns an error wrapping router.ErrFrozen.
func (a *App) Use(middleware httpserver.Middleware) error {
	if middleware == nil {
		return errors.New("HTTP middleware is required")
	}
	a.middlewareMu.Lock()
	defer a.middlewareMu.Unlock()
	if a.serving.Load() {
		return fmt.Errorf("add HTTP middleware: %w", router.ErrFrozen)
	}
	middlewares := append(slices.Clone(a.middlewares), middleware)
	var handler http.Handler = a.router
	var rewrites []httpserver.Rewriter
	for _, current := range slices.Backward(middlewares) {
		if handler = current(handler); handler == nil {
			return errors.New("HTTP middleware returned a nil handler")
		}
		if rewriter, ok := handler.(httpserver.Rewriter); ok {
			rewrites = append(rewrites, rewriter)
		}
	}
	slices.Reverse(rewrites)
	a.middlewares = middlewares
	a.chain.Store(&pipeline{handler: handler, rewrites: rewrites})
	return nil
}

// rewrite runs before the middleware given to WithHTTPMiddleware: it freezes
// the middleware and routes on the first request, then applies the request
// rewrites of the middleware added with Use, which therefore always match
// the handler dispatch serves.
func (a *App) rewrite(r *http.Request) *http.Request {
	if !a.serving.Load() {
		a.middlewareMu.Lock()
		if !a.serving.Load() {
			a.mountOpenAPI()
			a.serving.Store(true)
		}
		a.middlewareMu.Unlock()
	}
	for _, rewriter := range a.chain.Load().rewrites {
		r = rewriter.RewriteRequest(r)
	}
	return r
}

func (a *App) dispatch(w http.ResponseWriter, r *http.Request) {
	a.chain.Load().handler.ServeHTTP(w, r)
}

// mountOpenAPI registers GET OpenAPIPath just before the first request, when
// every application route is known, unless the configuration does not serve
// the document or the application has its own route for the path (an
// application that published a hand-written document keeps it).
func (a *App) mountOpenAPI() {
	if !a.servesOpenAPI() {
		return
	}
	if err := a.router.Handle("GET "+OpenAPIPath, openAPIHandler{a.serveOpenAPI}); err != nil {
		a.logger.Error("mount the OpenAPI document", "path", OpenAPIPath, "error", err)
	}
}

// Resources registers the conventional resource routes for the actions
// handler implements; see router.Router.Resources.
func (a *App) Resources(path string, handler any, middleware ...router.ResourceMiddleware) error {
	return a.router.Resources(path, handler, middleware...)
}

// Handle registers handler for pattern, a net/http.ServeMux pattern such as "GET /posts/{id}". It
// returns an error for an invalid or conflicting pattern, or once the application has served its
// first request.
func (a *App) Handle(pattern string, handler http.Handler) error {
	return a.router.Handle(pattern, handler)
}

// HandleFunc is Handle for a handler function.
func (a *App) HandleFunc(pattern string, handler http.HandlerFunc) error {
	return a.router.HandleFunc(pattern, handler)
}

// Readiness registers a named readiness check. GET /health/ready and GET /health run every check
// and answer 503 while any of them fails. Names must be unique.
func (a *App) Readiness(name string, check health.Check) error {
	return a.health.Register(name, check)
}

// Routes returns the registered routes sorted by path, then method.
func (a *App) Routes() []router.Route { return a.router.Routes() }

// SetAPIInfo sets the title, version and description of the application's
// OpenAPI document; the title defaults to "Forge application" and the
// version to "0.0.0".
func (a *App) SetAPIInfo(info openapi.Info) error {
	if info.Title == "" || info.Version == "" {
		return errors.New("the API needs a title and a version")
	}
	a.apiMu.Lock()
	defer a.apiMu.Unlock()
	a.apiInfo = info
	return nil
}

// OpenAPI builds the OpenAPI 3.1 document of the routes registered so far
// whose handlers are described (openapi.Describe, openapi.Typed); the
// others are listed by Document.Undocumented. It works whether or not the
// document is served, which is how `forge openapi` and tests use it.
func (a *App) OpenAPI() (*openapi.Document, error) {
	a.apiMu.Lock()
	info := a.apiInfo
	a.apiMu.Unlock()
	var routes []openapi.Route
	for _, route := range a.router.Routes() {
		handler, _ := a.router.Lookup(route)
		if route.Method == http.MethodGet && route.Pattern == OpenAPIPath && isServeOpenAPI(handler) {
			continue
		}
		routes = append(routes, openapi.Route{Method: route.Method, Pattern: route.Pattern, Handler: handler})
	}
	return openapi.Build(info, routes)
}

// servedOpenAPI returns the document served at OpenAPIPath. It is built on
// the first request for it: that request passed through the router, which
// froze it, so no route can be added afterwards.
func (a *App) servedOpenAPI() (*openapi.Document, error) {
	a.apiMu.Lock()
	served := a.apiServed
	a.apiMu.Unlock()
	if served != nil {
		return served, nil
	}
	document, err := a.OpenAPI()
	if err != nil {
		return nil, err
	}
	a.apiMu.Lock()
	defer a.apiMu.Unlock()
	if a.apiServed == nil {
		a.apiServed, a.apiHandler = document, document.Handler()
	}
	return a.apiServed, nil
}

// servesOpenAPI reports whether Forge serves its document: the configuration
// allows it and no application route owns OpenAPIPath.
func (a *App) servesOpenAPI() bool {
	if !a.config.ServesOpenAPI() {
		return false
	}
	for _, route := range a.router.Routes() {
		if route.Pattern == OpenAPIPath {
			handler, _ := a.router.Lookup(route)
			if !isServeOpenAPI(handler) {
				return false
			}
		}
	}
	return true
}

// openAPIHandler marks Forge's own document route, so App.OpenAPI leaves it
// out and documents an application route at the same path.
type openAPIHandler struct{ http.HandlerFunc }

func isServeOpenAPI(handler http.Handler) bool {
	_, ok := handler.(openAPIHandler)
	return ok
}

func (a *App) serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	if _, err := a.servedOpenAPI(); err != nil {
		web.Error(w, r, fmt.Errorf("build the OpenAPI document: %w", err))
		return
	}
	a.apiMu.Lock()
	handler := a.apiHandler
	a.apiMu.Unlock()
	handler.ServeHTTP(w, r)
}

// Handler returns the application's full HTTP handler, including the security headers and the
// middleware, without listening. Use it to test the application with httptest.
func (a *App) Handler() http.Handler { return a.server.Handler() }

// Run serves HTTP until ctx is cancelled, then drains in-flight requests and
// runs the shutdown hooks. Draining and hooks share one budget,
// FORGE_HTTP_SHUTDOWN_TIMEOUT, counted from cancellation, so the whole
// shutdown fits inside an orchestrator grace period set slightly above it.
func (a *App) Run(ctx context.Context) error {
	var stoppedAt atomic.Pointer[time.Time]
	stopWatching := context.AfterFunc(ctx, func() {
		now := time.Now()
		stoppedAt.Store(&now)
	})
	defer stopWatching()

	if a.config.ServesOpenAPI() && !a.servesOpenAPI() {
		a.logger.Info("the application serves its own " + OpenAPIPath + "; Forge's OpenAPI document is not mounted there")
	}
	if a.servesOpenAPI() {
		// An inaccurate description is a startup error, not a 500 on
		// the first request. The served document is built later, from
		// the routes as they are when the router freezes.
		if _, err := a.OpenAPI(); err != nil {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.config.HTTP.ShutdownTimeout)
			defer cancel()
			return errors.Join(err, a.Close(closeCtx))
		}
		if a.config.Environment != config.Development && a.config.Environment != config.Test {
			a.logger.Warn("serving the OpenAPI document, which lists every documented route; FORGE_OPENAPI_ENABLED=true enabled it", "path", OpenAPIPath, "environment", a.config.Environment)
		}
	}
	var runErr error
	stopMetrics, err := a.serveMetrics(ctx)
	if err == nil {
		runErr = errors.Join(a.server.Run(ctx), stopMetrics())
	} else {
		runErr = err
	}
	deadline := time.Now().Add(a.config.HTTP.ShutdownTimeout)
	if stopped := stoppedAt.Load(); stopped != nil {
		deadline = stopped.Add(a.config.HTTP.ShutdownTimeout)
	}
	closeCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	return errors.Join(runErr, a.Close(closeCtx))
}

// liveness is the body of GET /health/live.
type liveness struct {
	Status string `json:"status" doc:"Always \"live\" while the process serves requests."`
}

func (a *App) registerHealthRoutes() error {
	live := openapi.Typed(http.StatusOK, openapi.Operation{
		ID:      "healthLive",
		Summary: "Liveness: the process is serving requests",
		Tags:    []string{"health"},
	}, func(*http.Request, openapi.NoBody) (liveness, error) { return liveness{Status: "live"}, nil })
	if err := a.router.Handle("GET "+liveRoutePath, live); err != nil {
		return err
	}
	ready := func(w http.ResponseWriter, r *http.Request) {
		result := a.health.Ready(r.Context())
		status := http.StatusOK
		if !result.Healthy {
			status = http.StatusServiceUnavailable
		}
		web.JSON(w, status, result)
	}
	readiness := func(id string) http.Handler {
		return openapi.Describe(http.HandlerFunc(ready), openapi.Operation{
			ID:      id,
			Summary: "Readiness: every registered check passes",
			Tags:    []string{"health"},
			Responses: []openapi.Response{
				openapi.JSON[health.Result](http.StatusOK),
				{Status: http.StatusServiceUnavailable, Description: "A check failed", Body: openapi.TypeOf[health.Result]()},
			},
		})
	}
	if err := a.router.Handle("GET /health/ready", readiness("healthReady")); err != nil {
		return err
	}
	return a.router.Handle("GET /health", readiness("health"))
}

const (
	metricsPath            = "/metrics"
	metricsShutdownTimeout = 5 * time.Second
)

// serveMetrics starts the Prometheus listener on FORGE_TELEMETRY_METRICS_ADDR,
// if one is configured, and returns the function that stops it. The listener
// is separate from the application's so metrics are never reachable through
// the public router; it serves GET /metrics only, over plain HTTP, without
// authentication, and must be bound to a private interface.
func (a *App) serveMetrics(ctx context.Context) (func() error, error) {
	address := a.config.Telemetry.MetricsAddress
	if a.telemetry == nil || address == "" {
		return func() error { return nil }, nil
	}
	if !a.config.Telemetry.MetricsLoopback() && !a.config.Telemetry.MetricsPublic {
		a.logger.Warn("the metrics listener has no authentication and is not bound to loopback; keep its port off public networks and set FORGE_TELEMETRY_METRICS_PUBLIC=true to confirm (production refuses to start otherwise)", "address", address)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for metrics: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle(metricsPath, a.telemetry.Metrics().PrometheusHandler())
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	done := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error("metrics listener stopped", "error", err)
		}
		done <- err
	}()
	a.logger.Info("serving metrics", "address", listener.Addr().String(), "path", metricsPath)
	return func() error {
		// Called after ctx has ended: keep its values, not its cancellation.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), metricsShutdownTimeout)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(fmt.Errorf("shutdown metrics listener: %w", shutdownErr), server.Close())
		}
		if serveErr := <-done; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return errors.Join(fmt.Errorf("serve metrics: %w", serveErr), shutdownErr)
		}
		return shutdownErr
	}, nil
}
