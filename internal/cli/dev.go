package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/internal/devloop"
	"github.com/rocketinsidecorp/forge-go/internal/frontend"
)

const (
	// devReadyTimeout bounds how long a started application may take to
	// accept connections before the dev runner reports it as not starting.
	devReadyTimeout = 30 * time.Second
	// buildOutputLimit bounds the compiler output kept for display.
	buildOutputLimit = 64 << 10
	minPollInterval  = 50 * time.Millisecond
	maxPollInterval  = time.Minute
)

type devOptions struct {
	name         string
	noReload     bool
	pollInterval time.Duration
	debounce     time.Duration
}

// runDev serves the application with development settings. By default it
// watches the source tree, rebuilds on change, restarts the application
// gracefully and reloads the browser (ADR 0035).
func runDev(args []string, stdout, stderr io.Writer) error {
	options, err := parseDevArgs(args, stderr)
	if err != nil {
		return err
	}
	name, err := resolveCommand(".", optionalArg(options.name))
	if err != nil {
		return err
	}
	env, err := developmentEnvironment(".")
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if options.noReload {
		return runAppWithInput(ctx, ".", name, nil, env, nil, stdout, stderr)
	}
	return devLoop(ctx, ".", name, env, options, stdout, stderr)
}

func optionalArg(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

func parseDevArgs(args []string, stderr io.Writer) (devOptions, error) {
	flags := flag.NewFlagSet("dev", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := devOptions{}
	flags.BoolVar(&options.noReload, "no-reload", false, "run the application once: no file watching, restarts or browser reload")
	flags.DurationVar(&options.pollInterval, "poll-interval", devloop.DefaultPollInterval, "how often the source tree is scanned for changes")
	flags.DurationVar(&options.debounce, "debounce", devloop.DefaultDebounce, "quiet time after a change before rebuilding")
	// The command name may come before or after the flags.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		options.name, args = args[0], args[1:]
	}
	if err := parseFlags(flags, args); err != nil {
		return devOptions{}, err
	}
	if flags.NArg() > 1 || (flags.NArg() == 1 && options.name != "") {
		return devOptions{}, usageError{"usage: forge dev [NAME] [--no-reload] [--poll-interval D] [--debounce D]"}
	}
	if flags.NArg() == 1 {
		options.name = flags.Arg(0)
	}
	if options.pollInterval < minPollInterval || options.pollInterval > maxPollInterval {
		return devOptions{}, fmt.Errorf("--poll-interval must be between %s and %s", minPollInterval, maxPollInterval)
	}
	if options.debounce < 0 || options.debounce > maxPollInterval {
		return devOptions{}, fmt.Errorf("--debounce must be between 0 and %s", maxPollInterval)
	}
	return options, nil
}

// devLoop is `forge dev` with live reload: the watcher, the supervisor and,
// in development over plain HTTP, the browser-reload proxy.
func devLoop(ctx context.Context, root, name string, env []string, options devOptions, stdout, stderr io.Writer) error {
	logf := func(format string, args ...any) {
		fmt.Fprintf(stderr, "forge dev %s: %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	}
	buildDir, err := os.MkdirTemp("", "forge-dev-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(buildDir) }()

	proxy, listener, err := startReloadProxy(ctx, env, logf)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var served sync.WaitGroup
	if proxy != nil {
		served.Go(func() {
			if err := proxy.Serve(runCtx, listener); err != nil {
				logf("reload proxy stopped: %v", err)
			}
		})
		defer func() { cancel(); served.Wait() }()
	}

	changes, err := devloop.Watch(runCtx, devloop.Options{Root: root, PollInterval: options.pollInterval, Debounce: options.debounce, Logf: logf})
	if err != nil {
		return fmt.Errorf("watch %s: %w", root, err)
	}

	app := &devApp{root: root, name: name, proxy: proxy, stdout: stdout, stderr: stderr, logf: logf}
	if proxy != nil {
		app.publicAddr = listener.Addr().String()
	}
	runner := &devloop.Runner{
		Dir: buildDir, Name: name,
		Build: app.build, Start: app.start, Ready: app.ready,
		Logf:     logf,
		Observer: app.observer(),
	}
	if proxy == nil {
		logf("watching %s; the application restarts on change (no browser reload)", root)
	} else {
		logf("watching %s; open http://%s (browser reload on)", root, app.publicAddr)
	}
	return runner.Run(runCtx, changes)
}

// startReloadProxy returns the proxy and its listener, or nil when browser
// reload does not apply: outside development, over TLS or behind a trusted
// proxy, or when the configuration does not load (the application reports
// that itself). The listener takes the address the application is configured
// for, so the URL people know keeps working; the application moves to a
// private loopback port.
func startReloadProxy(ctx context.Context, env []string, logf func(string, ...any)) (*devloop.Proxy, net.Listener, error) {
	cfg, err := config.LoadWithLookup(lookupIn(env))
	if err != nil {
		logf("browser reload is off: the configuration is invalid (%v); fix it and the application restarts, then restart forge dev for reload", err)
		return nil, nil, nil
	}
	if cfg.Environment != config.Development {
		logf("browser reload is off: it runs only with FORGE_ENV=development (FORGE_ENV=%s)", cfg.Environment)
		return nil, nil, nil
	}
	if cfg.HTTP.Transport != config.TransportPlain {
		logf("browser reload is off: it needs FORGE_HTTP_TRANSPORT=plain (it is %s)", cfg.HTTP.Transport)
		return nil, nil, nil
	}
	proxy, err := devloop.NewProxy(devloop.ProxyOptions{
		Environment:     string(cfg.Environment),
		ShowBuildErrors: loopbackAddress(cfg.HTTP.Address),
		Logf:            logf,
	})
	if err != nil {
		return nil, nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(context.WithoutCancel(ctx), "tcp", cfg.HTTP.Address)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on %s (FORGE_HTTP_ADDR): %w", cfg.HTTP.Address, err)
	}
	return proxy, listener, nil
}

// loopbackAddress reports whether address binds only this machine; ":8080"
// binds every interface.
func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// devApp is the runner's view of the application: how to build it, start it
// and tell the browser. The runner calls these from one goroutine.
type devApp struct {
	root, name string
	proxy      *devloop.Proxy
	publicAddr string
	stdout     io.Writer
	stderr     io.Writer
	logf       func(string, ...any)

	upstream string // the private address of the latest started application
	running  bool
}

// build compiles the application. The TypeScript front end is compiled for
// development (inline source maps) into a temporary directory and handed to
// `go build -overlay`, so the binary embeds it where app/javascript/dist is
// while the working tree, and the production output committed in it, stay as
// they are. A TypeScript syntax error is a build failure like a Go one.
func (a *devApp) build(ctx context.Context, output string) error {
	var out cappedBuffer
	args := []string{"build"}
	scratch, err := os.MkdirTemp("", "forge-dev-js-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	overlay, err := frontend.Overlay(a.root, scratch)
	if err != nil {
		return &devloop.BuildError{Output: err.Error()}
	}
	if overlay != "" {
		args = append(args, "-overlay", overlay)
	}
	command := goCommand(ctx, a.root, nil, &out, &out, append(args, "-o", output, "./cmd/"+a.name)...)
	devloop.IsolateCommand(command) // cancelling the build ends the compiler and linker too
	err = command.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &devloop.BuildError{Output: strings.TrimSpace(out.String())}
}

func (a *devApp) start(ctx context.Context, binary string) (devloop.Instance, error) {
	// The environment is read again on every start, so an edited .env.local
	// takes effect on the restart it triggers.
	env, err := developmentEnvironment(a.root)
	if err != nil {
		return nil, err
	}
	if a.proxy != nil {
		port, err := freeLoopbackPort(ctx)
		if err != nil {
			return nil, err
		}
		a.upstream = port
		env = append(env, "FORGE_HTTP_ADDR="+port)
	}
	return devloop.StartProcess(binary, nil, a.root, env, a.stdout, a.stderr)
}

// ready waits until the application accepts connections, so the browser is
// told to reload only when the page can load.
func (a *devApp) ready(ctx context.Context, instance devloop.Instance) error {
	if a.proxy == nil {
		return nil
	}
	deadline := time.NewTimer(devReadyTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		dialer := net.Dialer{Timeout: time.Second}
		if conn, err := dialer.DialContext(ctx, "tcp", a.upstream); err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-instance.Done():
			return errors.New("the application exited before it served; fix the error above and save")
		case <-deadline.C:
			return fmt.Errorf("the application did not listen on %s within %s", a.upstream, devReadyTimeout)
		case <-tick.C:
		}
	}
}

func (a *devApp) observer() devloop.Observer {
	obs := devloop.Observer{
		Stopping: func() { a.running = false },
		Started: func(devloop.Instance) {
			a.running = true
			if a.proxy != nil {
				a.proxy.SetUpstream(a.upstream)
				a.proxy.Reload()
			}
		},
	}
	if a.proxy == nil {
		return obs
	}
	obs.Stopping = func() {
		a.running = false
		a.proxy.Restarting()
	}
	obs.BuildFailed = func(output string) {
		if !a.running {
			a.proxy.Unavailable("The build failed")
		}
		a.proxy.BuildFailed(output)
	}
	obs.Crashed = func() {
		a.running = false
		a.proxy.Unavailable("The application exited")
	}
	obs.StartFailed = func(error) {
		a.running = false
		a.proxy.Unavailable("The application did not start")
	}
	return obs
}

// freeLoopbackPort returns a loopback address with a currently free port.
func freeLoopbackPort(ctx context.Context) (string, error) {
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("find a free port: %w", err)
	}
	address := l.Addr().String()
	if err := l.Close(); err != nil {
		return "", err
	}
	return address, nil
}

// cappedBuffer keeps the first buildOutputLimit bytes written to it.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	written := len(p)
	room := buildOutputLimit - c.buf.Len()
	if room < len(p) {
		c.truncated = true
		p = p[:max(room, 0)]
	}
	c.buf.Write(p)
	return written, nil // never a short write: the compiler must not see an error
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return c.buf.String() + "\n... output truncated"
	}
	return c.buf.String()
}
