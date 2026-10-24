package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// Default paths inside the forge-proxy image.
const (
	DefaultControlSocket = "/run/forge-proxy/control.sock"
	DefaultStatePath     = "/var/lib/forge-proxy/state.json"
	DefaultACMECache     = "/var/lib/forge-proxy/acme"
)

// Exit codes of the forge-proxy client commands.
const (
	ExitOK        = 0
	ExitFailed    = 1
	ExitUsage     = 2
	ExitUnhealthy = 4
)

// Main runs the forge-proxy command line: `run` (the default) serves
// traffic; deploy, remove, maintenance, status and healthcheck talk to a
// running proxy over its control socket.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	switch command {
	case "run":
		if err := run(ctx, args, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "forge-proxy: %v\n", err)
			return ExitFailed
		}
		return ExitOK
	case "deploy", "remove", "maintenance", "status", "healthcheck":
		return client(ctx, command, args, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "forge-proxy: unknown command %q (run, deploy, remove, maintenance, status, healthcheck)\n", command)
		return ExitUsage
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	httpAddr := flags.String("http-addr", ":8080", "plain HTTP listen address (publish as host port 80)")
	httpsAddr := flags.String("https-addr", ":8443", "HTTPS listen address (publish as host port 443); empty disables HTTPS and ACME")
	controlPath := flags.String("control", DefaultControlSocket, "control socket path")
	statePath := flags.String("state", DefaultStatePath, "routing state file; empty disables persistence")
	acmeCache := flags.String("acme-cache", DefaultACMECache, "directory caching ACME account keys and certificates")
	acmeEmail := flags.String("acme-email", "", "contact address registered with the ACME CA")
	acmeDirectory := flags.String("acme-directory", "", "ACME directory URL (default: Let's Encrypt production)")
	readHeaderTimeout := flags.Duration("read-header-timeout", 10*time.Second, "time allowed to read request headers")
	idleTimeout := flags.Duration("idle-timeout", 120*time.Second, "keep-alive idle timeout; also how long a write to a client may make no progress and an upgraded connection may carry no data")
	shutdownTimeout := flags.Duration("shutdown-timeout", 30*time.Second, "graceful shutdown budget")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("run accepts flags only")
	}
	if *idleTimeout <= 0 {
		return errors.New("-idle-timeout must be positive")
	}
	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	server, err := New(logger, *statePath)
	if err != nil {
		return err
	}
	server.idleTimeout = *idleTimeout
	// Everything that can refuse the configuration runs before the control
	// socket exists.
	if *httpsAddr != "" {
		if err := privateDir(*acmeCache); err != nil {
			return fmt.Errorf("ACME cache: %w", err)
		}
	}
	controlListener, err := ListenControl(*controlPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 3)
	go func() { errs <- server.ServeControl(ctx, controlListener) }()

	var handler http.Handler = server
	var servers []*http.Server
	if *httpsAddr != "" {
		manager := &autocert.Manager{
			Prompt: autocert.AcceptTOS,
			Cache:  durableCache{autocert.DirCache(*acmeCache)},
			Email:  *acmeEmail,
			HostPolicy: func(_ context.Context, host string) error {
				if server.TLSHost(host) {
					return nil
				}
				return fmt.Errorf("host %q is not configured for TLS", host)
			},
		}
		if *acmeDirectory != "" {
			manager.Client = &acme.Client{DirectoryURL: *acmeDirectory}
		}
		tlsConfig := manager.TLSConfig()
		tlsConfig.MinVersion = tls.VersionTLS12
		handler = manager.HTTPHandler(server)
		servers = append(servers, newHTTPServer(*httpsAddr, server, tlsConfig, *readHeaderTimeout, *idleTimeout, logger))
	}
	plain := newHTTPServer(*httpAddr, handler, nil, *readHeaderTimeout, *idleTimeout, logger)
	servers = append([]*http.Server{plain}, servers...)
	for _, srv := range servers {
		listener, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return err
		}
		go func() {
			var serveErr error
			if srv.TLSConfig != nil {
				serveErr = srv.ServeTLS(listener, "", "")
			} else {
				serveErr = srv.Serve(listener)
			}
			if !errors.Is(serveErr, http.ErrServerClosed) {
				errs <- serveErr
			}
		}()
	}
	logger.Info("forge-proxy started", "http_addr", *httpAddr, "https_addr", *httpsAddr, "control", *controlPath)
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errs:
	}
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer stop()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	logger.Info("forge-proxy stopped")
	return runErr
}

func newHTTPServer(addr string, handler http.Handler, tlsConfig *tls.Config, readHeader, idle time.Duration, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: readHeader,
		IdleTimeout:       idle,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

func client(ctx context.Context, command string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	controlPath := flags.String("control", DefaultControlSocket, "control socket path")
	request := ControlRequest{Op: command}
	var deploy DeployRequest
	var hosts string
	var healthInterval, healthTimeout, deployTimeout, drainTimeout, responseTimeout time.Duration
	switch command {
	case "deploy":
		flags.StringVar(&deploy.Service, "service", "", "service name")
		flags.StringVar(&deploy.Target, "target", "", "new target host:port")
		flags.StringVar(&hosts, "hosts", "", "comma-separated host names")
		flags.BoolVar(&deploy.TLS, "tls", false, "obtain certificates and redirect HTTP to HTTPS")
		flags.StringVar(&deploy.HealthPath, "health-path", DefaultHealthPath, "health check path")
		flags.DurationVar(&healthInterval, "health-interval", DefaultHealthInterval, "time between health checks")
		flags.DurationVar(&healthTimeout, "health-timeout", DefaultHealthTimeout, "timeout of one health check")
		flags.IntVar(&deploy.HealthyCount, "healthy-count", DefaultHealthyCount, "consecutive passing checks required")
		flags.DurationVar(&deployTimeout, "deploy-timeout", DefaultDeployTimeout, "time allowed to become healthy")
		flags.DurationVar(&drainTimeout, "drain-timeout", DefaultDrainTimeout, "time allowed for in-flight requests on the previous target")
		flags.DurationVar(&responseTimeout, "response-timeout", DefaultResponseTimeout, "time allowed for the target to send response headers")
		flags.Int64Var(&deploy.MaxBodyBytes, "max-body-bytes", DefaultMaxBodyBytes, "largest accepted request body")
		flags.Int64Var(&deploy.BufferBytes, "buffer-bytes", DefaultBufferBytes, "largest request body buffered before forwarding")
	case "remove":
		flags.StringVar(&request.Service, "service", "", "service name")
		flags.DurationVar(&drainTimeout, "drain-timeout", DefaultDrainTimeout, "time allowed for in-flight requests")
	case "maintenance":
		flags.StringVar(&request.Service, "service", "", "service name")
	}
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	rest := flags.Args()
	switch command {
	case "deploy":
		if hosts != "" {
			deploy.Hosts = strings.Split(hosts, ",")
		}
		deploy.HealthInterval, deploy.HealthTimeout = Duration(healthInterval), Duration(healthTimeout)
		deploy.DeployTimeout, deploy.DrainTimeout, deploy.ResponseTimeout = Duration(deployTimeout), Duration(drainTimeout), Duration(responseTimeout)
		request.Deploy = &deploy
	case "remove":
		request.DrainTimeout = Duration(drainTimeout)
	case "maintenance":
		if len(rest) != 1 || (rest[0] != "on" && rest[0] != "off") {
			fmt.Fprintln(stderr, "usage: forge-proxy maintenance -service NAME on|off")
			return ExitUsage
		}
		request.Maintenance = rest[0] == "on"
		rest = nil
	}
	if len(rest) != 0 {
		fmt.Fprintf(stderr, "forge-proxy %s: unexpected arguments\n", command)
		return ExitUsage
	}
	timeout := 30 * time.Second
	switch command {
	case "deploy":
		timeout = deployTimeout + drainTimeout + 30*time.Second
	case "remove":
		timeout = drainTimeout + 30*time.Second
	case "healthcheck":
		request.Op = "status"
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := Call(ctx, *controlPath, request)
	if err != nil {
		fmt.Fprintf(stderr, "forge-proxy: %v\n", err)
		return ExitFailed
	}
	if command != "healthcheck" {
		_ = json.NewEncoder(stdout).Encode(response)
	}
	if !response.OK {
		fmt.Fprintf(stderr, "forge-proxy: %s\n", response.Error)
		if response.Code == "unhealthy" {
			return ExitUnhealthy
		}
		return ExitFailed
	}
	return ExitOK
}
