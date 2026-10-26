// Package httpserver owns Forge's secure HTTP server defaults and lifecycle.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

// Server is Forge's HTTP server: the standard library server behind a fixed chain of request ID,
// client address, security headers, body limit and panic recovery.
type Server struct {
	server          *http.Server
	transport       config.TransportMode
	certFile        string
	keyFile         string
	shutdownTimeout time.Duration
	logger          *slog.Logger
}

// Middleware wraps an HTTP handler. Middleware is applied in the order passed
// to NewWithMiddleware: the first middleware is the outermost custom layer.
// Forge's request ID, client address resolution (web.ClientIP) and security
// headers remain outside custom middleware; custom middleware remains inside
// body limiting and panic recovery.
type Middleware func(http.Handler) http.Handler

// Rewriter is implemented by a handler a Middleware returns when the
// middleware rewrites the request for routing, as i18n.Translator.Middleware
// does with NegotiationOptions.PathPrefix (/pt-BR/admin is routed as
// /admin). The server calls RewriteRequest of every such middleware, in
// registration order, once per request and before the first application
// middleware runs, so every middleware sees the request path the router
// uses whatever its position: a path guard registered before the rewriting
// middleware cannot be bypassed by the part the rewrite removes.
// RewriteRequest returns a non-nil request, must be idempotent for the
// request it returns (the middleware's ServeHTTP may apply it again) and
// cannot answer the request.
// Request IDs, telemetry and the panic log run before it and see the request
// as sent.
type Rewriter interface {
	RewriteRequest(r *http.Request) *http.Request
}

// New returns a Server for handler with the configuration's timeouts, limits and transport.
func New(cfg config.Config, handler http.Handler, logger *slog.Logger) (*Server, error) {
	return NewWithMiddleware(cfg, handler, logger)
}

// NewWithMiddleware constructs the standard Forge server with optional
// application middleware around the handler.
func NewWithMiddleware(
	cfg config.Config,
	handler http.Handler,
	logger *slog.Logger,
	middlewares ...Middleware,
) (*Server, error) {
	return NewWithOptions(cfg, handler, logger, Options{Middlewares: middlewares})
}

// Options configure NewWithOptions.
type Options struct {
	// Instrument wraps the chain directly inside the request-ID layer and
	// outside panic recovery, so it observes every response, including
	// recovered panics and 413s, and can extend the request logger. Forge
	// installs its telemetry middleware here. Nil means none.
	Instrument Middleware
	// Middlewares are application middleware, as for NewWithMiddleware.
	Middlewares []Middleware
	// Rewrite runs once per request after the rewrites of Middlewares that
	// return a Rewriter and before the first of Middlewares. forge.App runs
	// the rewrites of the middleware added with App.Use here. Nil means
	// none.
	Rewrite func(*http.Request) *http.Request
}

// NewWithOptions constructs the standard Forge server with an optional
// instrumentation layer and application middleware.
func NewWithOptions(cfg config.Config, handler http.Handler, logger *slog.Logger, opts Options) (*Server, error) {
	middlewares := opts.Middlewares
	if handler == nil || logger == nil {
		return nil, errors.New("HTTP handler and logger are required")
	}
	for _, middleware := range middlewares {
		if middleware == nil {
			return nil, errors.New("HTTP middleware is required")
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	wrappedHandler := handler
	var rewrites []func(*http.Request) *http.Request
	if opts.Rewrite != nil {
		rewrites = append(rewrites, opts.Rewrite)
	}
	for i := len(middlewares) - 1; i >= 0; i-- {
		wrappedHandler = middlewares[i](wrappedHandler)
		if wrappedHandler == nil {
			return nil, errors.New("HTTP middleware returned a nil handler")
		}
		if rewriter, ok := wrappedHandler.(Rewriter); ok {
			rewrites = append(rewrites, rewriter.RewriteRequest)
		}
	}
	// Collected innermost first; they run in registration order.
	slices.Reverse(rewrites)
	// Outermost first: every response, including 413 and recovered panics,
	// carries the security headers and the request ID.
	inner := recoverer(limitBody(cfg.HTTP.MaxBodyBytes, rewrite(rewrites, wrappedHandler)))
	if opts.Instrument != nil {
		if inner = opts.Instrument(inner); inner == nil {
			return nil, errors.New("HTTP instrumentation returned a nil handler")
		}
	}
	// The client address is resolved once, before anything reads it
	// (instrumentation, rate limits, login throttles, audit).
	trusted, err := web.NewTrustedProxies(cfg.HTTP.TrustedProxies...)
	if err != nil {
		return nil, fmt.Errorf("invalid FORGE_HTTP_TRUSTED_PROXIES: %w", err)
	}
	wrapped := securityHeaders(cfg, trusted.Middleware(requestID(logger, inner)))
	return &Server{
		server: &http.Server{
			Addr:              cfg.HTTP.Address,
			Handler:           wrapped,
			ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
			ReadTimeout:       cfg.HTTP.ReadTimeout,
			WriteTimeout:      cfg.HTTP.WriteTimeout,
			IdleTimeout:       cfg.HTTP.IdleTimeout,
			MaxHeaderBytes:    cfg.HTTP.MaxHeaderBytes,
		},
		transport:       cfg.HTTP.Transport,
		certFile:        cfg.HTTP.TLSCertFile,
		keyFile:         cfg.HTTP.TLSKeyFile,
		shutdownTimeout: cfg.HTTP.ShutdownTimeout,
		logger:          logger,
	}, nil
}

// Handler returns the server's complete handler, so it can be tested without listening.
func (s *Server) Handler() http.Handler { return s.server.Handler }

// Run serves until ctx is cancelled and then shuts down gracefully, waiting up to the configured
// shutdown timeout for in-flight requests and then closing their connections. It returns nil after
// a clean stop and an error for a listener failure or a forced close.
func (s *Server) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("run context is required")
	}
	// Listening here, rather than through ListenAndServe, lets the log below
	// report the address the kernel bound: with port 0 the configured address
	// does not say which port was chosen.
	address := s.server.Addr
	if address == "" {
		address = ":http" // what ListenAndServe uses for an empty address
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("serve HTTP: %w", err)
	}
	s.logger.Info("serving HTTP", "address", listener.Addr().String())
	errorsChannel := make(chan error, 1)
	go func() {
		if s.transport == config.TransportTLS {
			errorsChannel <- s.server.ServeTLS(listener, s.certFile, s.keyFile)
		} else {
			errorsChannel <- s.server.Serve(listener)
		}
	}()

	select {
	case err := <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		if err := s.server.Shutdown(shutdownContext); err != nil {
			// The drain deadline passed. Closing the connections cancels the
			// remaining handlers' request contexts, so work holding resources
			// such as database connections stops instead of outliving the
			// process's grace period.
			return errors.Join(fmt.Errorf("shutdown HTTP: %w", err), s.server.Close())
		}
		if err := <-errorsChannel; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP during shutdown: %w", err)
		}
		return nil
	}
}

func securityHeaders(cfg config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if cfg.HTTP.Transport != config.TransportPlain {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// rewrite applies every rewrite to the request before next, the first
// application middleware, sees it.
func rewrite(rewrites []func(*http.Request) *http.Request, next http.Handler) http.Handler {
	if len(rewrites) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, apply := range rewrites {
			r = apply(r)
		}
		next.ServeHTTP(w, r)
	})
}

func limitBody(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limit {
			web.Error(w, r, fault.New("body_too_large", "request body too large", fault.CategoryInvalid, http.StatusRequestEntityTooLarge))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// requestID assigns every request a fresh UUIDv7. Incoming X-Request-ID
// headers are not trusted. The ID and a logger carrying it are placed in the
// request context (see web.RequestID and web.Logger).
func requestID(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		generated, err := uuid.New()
		if err != nil {
			web.Error(w, r.WithContext(web.WithLogger(r.Context(), logger)), fault.New("internal_error", "internal server error", fault.CategoryInternal, http.StatusInternalServerError).WithCause(err))
			return
		}
		id := generated.String()
		w.Header().Set("X-Request-ID", id)
		ctx := web.WithRequestID(r.Context(), id)
		ctx = web.WithLogger(ctx, logger.With("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				web.Logger(r.Context()).Error("request panic", "method", r.Method, "path", r.URL.Path, "panic", recovered, "stack", string(debug.Stack()))
				web.JSON(w, http.StatusInternalServerError, web.ErrorBody{Error: web.ErrorDetail{
					Code: "internal_error", Message: "internal server error", RequestID: web.RequestID(r.Context()),
				}})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
