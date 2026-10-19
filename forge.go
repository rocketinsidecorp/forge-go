// Package forge composes the framework runtime.
package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/health"
	"github.com/rocketinsidecorp/forge-go/httpserver"
	"github.com/rocketinsidecorp/forge-go/router"
	htmlviews "github.com/rocketinsidecorp/forge-go/views"
	"github.com/rocketinsidecorp/forge-go/web"
)

const liveRoutePath = "/health/live"

// ViewRenderer executes server-side HTML templates with contextual escaping.
// It is an alias for views.Renderer, exposed here for generated apps that
// vendor the main Forge package.
type ViewRenderer = htmlviews.Renderer

// NewViewRenderer parses *.gohtml templates from templates.
func NewViewRenderer(templates fs.FS) (*ViewRenderer, error) {
	return htmlviews.New(templates)
}

type Option func(*options) error

type options struct {
	config          config.Config
	logger          *slog.Logger
	httpMiddlewares []httpserver.Middleware
}

type App struct {
	config config.Config
	logger *slog.Logger
	router *router.Router
	health *health.Registry
	server *httpserver.Server

	hooksMu       sync.Mutex
	shutdownHooks []ShutdownHook
	closed        bool

	// middlewareMu guards middlewares and serving's transition to true;
	// chain is the composed handler the server dispatches to.
	middlewareMu sync.Mutex
	middlewares  []httpserver.Middleware
	chain        atomic.Pointer[http.Handler]
	serving      atomic.Bool
}

// ShutdownHook releases a resource, such as a database pool, after the HTTP
// server has drained. ctx is bounded by the configured shutdown timeout.
type ShutdownHook func(ctx context.Context) error

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
		config: settings.config,
		logger: settings.logger,
		router: router.New(),
		health: health.New(),
	}
	if err := app.registerHealthRoutes(); err != nil {
		return nil, err
	}
	var routes http.Handler = app.router
	app.chain.Store(&routes)
	server, err := httpserver.NewWithMiddleware(app.config, http.HandlerFunc(app.dispatch), app.logger, settings.httpMiddlewares...)
	if err != nil {
		return nil, err
	}
	app.server = server
	return app, nil
}

func WithConfig(cfg config.Config) Option {
	return func(settings *options) error {
		settings.config = cfg
		return nil
	}
}

func WithLogger(logger *slog.Logger) Option {
	return func(settings *options) error {
		if logger == nil {
			return errors.New("logger is required")
		}
		settings.logger = logger
		return nil
	}
}

// WithHTTPMiddleware adds middleware to the application's HTTP handler chain.
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
// so a middleware constructor may run more than once. After the first
// request has been served, Use returns an error wrapping router.ErrFrozen.
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
	for _, current := range slices.Backward(middlewares) {
		if handler = current(handler); handler == nil {
			return errors.New("HTTP middleware returned a nil handler")
		}
	}
	a.middlewares = middlewares
	a.chain.Store(&handler)
	return nil
}

func (a *App) dispatch(w http.ResponseWriter, r *http.Request) {
	if !a.serving.Load() {
		a.middlewareMu.Lock()
		a.serving.Store(true)
		a.middlewareMu.Unlock()
	}
	(*a.chain.Load()).ServeHTTP(w, r)
}

// Resources registers the conventional resource routes for the actions
// handler implements; see router.Router.Resources.
func (a *App) Resources(path string, handler any, middleware ...router.ResourceMiddleware) error {
	return a.router.Resources(path, handler, middleware...)
}

func (a *App) Handle(pattern string, handler http.Handler) error {
	return a.router.Handle(pattern, handler)
}

func (a *App) HandleFunc(pattern string, handler http.HandlerFunc) error {
	return a.router.HandleFunc(pattern, handler)
}

func (a *App) Readiness(name string, check health.Check) error {
	return a.health.Register(name, check)
}

func (a *App) Routes() []router.Route { return a.router.Routes() }

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

	runErr := a.server.Run(ctx)
	deadline := time.Now().Add(a.config.HTTP.ShutdownTimeout)
	if stopped := stoppedAt.Load(); stopped != nil {
		deadline = stopped.Add(a.config.HTTP.ShutdownTimeout)
	}
	closeCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	return errors.Join(runErr, a.Close(closeCtx))
}

func (a *App) registerHealthRoutes() error {
	if err := a.router.HandleFunc("GET "+liveRoutePath, func(w http.ResponseWriter, _ *http.Request) {
		web.JSON(w, http.StatusOK, map[string]string{"status": "live"})
	}); err != nil {
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
	if err := a.router.HandleFunc("GET /health/ready", ready); err != nil {
		return err
	}
	return a.router.HandleFunc("GET /health", ready)
}
