package forge

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// Configure registers an application's routes, checks and dependencies on a
// freshly built App. It is the composition root between the framework runtime
// and application code. ctx is cancelled when the process is asked to stop,
// so slow startup work such as connecting to a database can be interrupted.
type Configure func(ctx context.Context, app *App) error

// Command is an additional subcommand of the application binary, such as the
// migration commands contributed by the postgres package. Commands ship inside
// the production image, so operational tasks need no extra tooling.
type Command struct {
	Name    string
	Summary string
	Run     func(ctx context.Context, env CommandEnv, args []string) error
}

// ErrUsage marks a Command error caused by invalid arguments; Execute exits
// with status 2 for it instead of 1. Wrap it: fmt.Errorf("%w: ...", ErrUsage).
var ErrUsage = errors.New("usage")

// CommandEnv is what a Command receives from the runtime.
type CommandEnv struct {
	Config config.Config
	Logger *slog.Logger
	Stdout io.Writer
	Stderr io.Writer
	// Stdin is the process's standard input (os.Stdin under Main), for
	// commands that read a secret piped to them or typed at a terminal.
	// Secrets never belong in arguments, which other local users can list.
	// It is nil when the caller of Execute provides none.
	Stdin io.Reader
	// LookupEnv reads the process environment, like os.LookupEnv, for
	// command-specific variables that are not part of Config. It is never
	// nil.
	LookupEnv func(string) (string, bool)
}

const healthcheckTimeout = 2 * time.Second

var reservedCommands = map[string]bool{"serve": true, "healthcheck": true, "help": true}

// Main is the conventional entrypoint for Forge applications:
//
//	func main() { forge.Main(bootstrap.Configure, postgres.Commands(db.Migrations())...) }
//
// The binary serves HTTP by default ("serve") and also answers "healthcheck",
// which lets minimal container images (no shell, no curl) run a Docker
// HEALTHCHECK against the process itself, plus any extra commands. Main exits
// the process.
func Main(configure Configure, commands ...Command) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := execute(ctx, os.Args[1:], os.LookupEnv, os.Stdin, os.Stdout, os.Stderr, configure, commands...)
	stop()
	os.Exit(code)
}

// Execute runs an application command and returns its exit code: 0 on
// success, 1 on failure, 2 on misuse. Cancelling ctx triggers a graceful
// shutdown of "serve". It exists so the full startup path can be tested
// without touching process state.
//
// Commands run by Execute receive no standard input (CommandEnv.Stdin is
// nil); Main passes os.Stdin.
func Execute(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer, configure Configure, commands ...Command) int {
	return execute(ctx, args, lookup, nil, stdout, stderr, configure, commands...)
}

func execute(ctx context.Context, args []string, lookup func(string) (string, bool), stdin io.Reader, stdout, stderr io.Writer, configure Configure, commands ...Command) int {
	if ctx == nil || lookup == nil || stdout == nil || stderr == nil || configure == nil {
		if stderr != nil {
			fmt.Fprintln(stderr, "forge: context, environment lookup, outputs and configure function are required")
		}
		return 2
	}
	extra, err := indexCommands(commands)
	if err != nil {
		fmt.Fprintf(stderr, "forge: %v\n", err)
		return 2
	}
	name := "serve"
	if len(args) > 0 {
		name = args[0]
		args = args[1:]
	}
	if name == "help" || name == "-h" || name == "--help" {
		printCommands(stdout, commands)
		return 0
	}
	command, isExtra := extra[name]
	if !isExtra && !reservedCommands[name] {
		fmt.Fprintf(stderr, "unknown command %q\n", name)
		printCommands(stderr, commands)
		return 2
	}
	if !isExtra && len(args) != 0 {
		fmt.Fprintf(stderr, "%s accepts no arguments\n", name)
		return 2
	}

	cfg, err := config.LoadWithLookup(lookup)
	if err != nil {
		fmt.Fprintf(stderr, "invalid configuration: %v\n", err)
		return 1
	}
	logger := newLogger(cfg.Environment, stdout)
	switch {
	case isExtra:
		env := CommandEnv{Config: cfg, Logger: logger, Stdout: stdout, Stderr: stderr, Stdin: stdin, LookupEnv: lookup}
		if err := command.Run(ctx, env, args); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", name, err)
			if errors.Is(err, ErrUsage) {
				return 2
			}
			return 1
		}
		return 0
	case name == "healthcheck":
		if err := probeLiveness(ctx, cfg.HTTP); err != nil {
			fmt.Fprintf(stderr, "unhealthy: %v\n", err)
			return 1
		}
		return 0
	default:
		return serve(ctx, cfg, logger, configure)
	}
}

func indexCommands(commands []Command) (map[string]Command, error) {
	index := make(map[string]Command, len(commands))
	for _, command := range commands {
		switch {
		case command.Name == "" || command.Run == nil:
			return nil, errors.New("commands need a name and a Run function")
		case reservedCommands[command.Name]:
			return nil, fmt.Errorf("command name %q is reserved", command.Name)
		case index[command.Name].Run != nil:
			return nil, fmt.Errorf("command %q registered twice", command.Name)
		}
		index[command.Name] = command
	}
	return index, nil
}

func printCommands(output io.Writer, commands []Command) {
	fmt.Fprintln(output, "commands:")
	fmt.Fprintf(output, "  %-12s %s\n", "serve", "serve HTTP until SIGINT/SIGTERM (default)")
	fmt.Fprintf(output, "  %-12s %s\n", "healthcheck", "probe the running server's liveness endpoint")
	for _, command := range commands {
		fmt.Fprintf(output, "  %-12s %s\n", command.Name, command.Summary)
	}
}

func serve(ctx context.Context, cfg config.Config, logger *slog.Logger, configure Configure) int {
	app, err := New(WithConfig(cfg), WithLogger(logger))
	if err != nil {
		logger.Error("build application", "error", err)
		return 1
	}
	// The application's logger also exports records when
	// FORGE_TELEMETRY_LOGS_EXPORTER=otlp.
	logger = app.Logger()
	if err := configure(ctx, app); err != nil {
		logger.Error("configure application", "error", err)
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.HTTP.ShutdownTimeout)
		defer cancel()
		// Release whatever Configure acquired before failing.
		if closeErr := app.Close(closeCtx); closeErr != nil {
			logger.Error("release resources", "error", closeErr)
		}
		return 1
	}
	logger.Info("starting HTTP server", "address", cfg.HTTP.Address, "environment", cfg.Environment, "transport", cfg.HTTP.Transport)
	if err := app.Run(ctx); err != nil {
		logger.Error("application stopped", "error", err)
		return 1
	}
	logger.Info("application stopped")
	return 0
}

// newLogger emits JSON where logs are machine-collected and readable text
// where a developer is watching the terminal.
func newLogger(environment config.Environment, output io.Writer) *slog.Logger {
	// Records logged with a traced context (logger.InfoContext(ctx, ...))
	// carry trace_id and span_id, and the OTLP log exporter encodes values
	// in the same format.
	if environment == config.Development || environment == config.Test {
		return slog.New(telemetry.NewOutputHandler(output, telemetry.LogFormatText, nil))
	}
	return slog.New(telemetry.NewOutputHandler(output, telemetry.LogFormatJSON, nil))
}

// probeLiveness calls the liveness endpoint over loopback on the configured
// port. It therefore requires the server to listen on loopback or on all
// interfaces (0.0.0.0), which is what the generated images do.
func probeLiveness(ctx context.Context, cfg config.HTTP) error {
	_, port, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return fmt.Errorf("parse HTTP address: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()

	scheme := "http"
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	if cfg.Transport == config.TransportTLS {
		scheme = "https"
		// The certificate is issued for the public hostname, not 127.0.0.1.
		// The probe only reads a status code from our own process over
		// loopback and sends no credentials, so verification adds nothing.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} // #nosec G402 -- loopback self-probe, see above
	}
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+net.JoinHostPort("127.0.0.1", port)+liveRoutePath, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return errors.New("liveness endpoint returned " + response.Status)
	}
	return nil
}
