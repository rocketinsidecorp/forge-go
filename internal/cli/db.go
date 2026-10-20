package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/internal/dbtasks"
	"github.com/rocketinsidecorp/forge-go/internal/devloop"
)

const dbUsage = "usage: forge db <create|drop|reset|setup|prepare|migrate|rollback|status|seed|schema:dump|schema:load|console> [flags]"

// appRunner runs commands of the application binary (migrate, seed, ...).
type appRunner interface {
	Run(ctx context.Context, args []string) error
	Close()
}

// dbCommand is one `forge db` invocation, with everything it touches
// injectable so the destructive paths can be tested.
type dbCommand struct {
	root        string
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	interactive bool
	// processEnv looks up the real process environment: the one place
	// FORGE_ALLOW_DESTRUCTIVE is honored, never an env file.
	processEnv func(string) (string, bool)
	// newApp builds the application once, on first use.
	newApp func(ctx context.Context) (appRunner, error)
	// target resolves the database from the development environment.
	target func() (*dbtasks.Target, error)

	app appRunner
}

// runDB is `forge db`.
func runDB(args []string, stdout, stderr io.Writer) error {
	d := &dbCommand{
		root: ".", stdin: os.Stdin, stdout: stdout, stderr: stderr,
		interactive: stdinIsTerminal(), processEnv: os.LookupEnv,
	}
	d.newApp = func(ctx context.Context) (appRunner, error) { return buildAppRunner(ctx, d.root, stdout, stderr) }
	d.target = func() (*dbtasks.Target, error) {
		env, err := developmentEnvironment(d.root)
		if err != nil {
			return nil, err
		}
		return dbtasks.NewTarget(d.root, lookupIn(env))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer d.close()
	return d.run(ctx, args)
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (d *dbCommand) close() {
	if d.app != nil {
		d.app.Close()
	}
}

type destructiveFlags struct{ forceProduction, yes bool }

func (d *dbCommand) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(dbUsage)
	}
	name, rest := args[0], args[1:]
	switch name {
	case "create":
		return d.noArgs(name, rest, func() error { return d.create(ctx) })
	case "drop":
		return d.destructive(name, rest, func(f destructiveFlags) error { return d.drop(ctx, f) })
	case "reset":
		return d.destructive(name, rest, func(f destructiveFlags) error { return d.reset(ctx, f) })
	case "setup":
		return d.noArgs(name, rest, func() error { return d.setup(ctx) })
	case "prepare":
		return d.noArgs(name, rest, func() error { return d.prepare(ctx) })
	case "migrate":
		return d.noArgs(name, rest, func() error { return d.delegate(ctx, "migrate") })
	case "status":
		return d.noArgs(name, rest, func() error { return d.delegate(ctx, "migrate", "status") })
	case "seed":
		return d.noArgs(name, rest, func() error { return d.delegate(ctx, "seed") })
	case "rollback":
		return d.rollback(ctx, rest)
	case "schema:dump":
		return d.noArgs(name, rest, func() error { return d.schemaDump(ctx) })
	case "schema:load":
		return d.destructive(name, rest, func(f destructiveFlags) error { return d.schemaLoad(ctx, f) })
	case "console":
		return d.console(ctx, rest)
	default:
		return fmt.Errorf("unknown db command %q\n%s", name, dbUsage)
	}
}

func (d *dbCommand) noArgs(name string, args []string, run func() error) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: forge db %s", name)
	}
	return run()
}

func (d *dbCommand) destructive(name string, args []string, run func(destructiveFlags) error) error {
	flags := flag.NewFlagSet("db "+name, flag.ContinueOnError)
	flags.SetOutput(d.stderr)
	var f destructiveFlags
	flags.BoolVar(&f.forceProduction, "force-production", false, "allow this in FORGE_ENV=production, after confirmation")
	flags.BoolVar(&f.yes, "yes", false, "in production, confirm without a prompt; needs FORGE_ALLOW_DESTRUCTIVE=true in the process environment")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: forge db %s [--force-production [--yes]]", name)
	}
	return run(f)
}

func (d *dbCommand) say(format string, args ...any) { fmt.Fprintf(d.stdout, format+"\n", args...) }

func (d *dbCommand) create(ctx context.Context) error {
	target, err := d.target()
	if err != nil {
		return err
	}
	created, err := target.Create(ctx)
	if err != nil {
		return err
	}
	if created {
		d.say("created %s", target.Describe())
	} else {
		d.say("%s already exists", target.Describe())
	}
	return nil
}

func (d *dbCommand) drop(ctx context.Context, f destructiveFlags) error {
	target, err := d.target()
	if err != nil {
		return err
	}
	if err := d.confirm(target, "drop", f); err != nil {
		return err
	}
	dropped, err := target.Drop(ctx)
	if err != nil {
		return err
	}
	if dropped {
		d.say("dropped %s", target.Describe())
	} else {
		d.say("%s does not exist", target.Describe())
	}
	return nil
}

// delegate runs a command of the application binary, so development and
// production share one migration engine.
func (d *dbCommand) delegate(ctx context.Context, args ...string) error {
	if d.app == nil {
		app, err := d.newApp(ctx)
		if err != nil {
			return err
		}
		d.app = app
	}
	return d.app.Run(ctx, args)
}

func (d *dbCommand) rollback(ctx context.Context, args []string) error {
	target, err := d.target()
	if err != nil {
		return err
	}
	// Rolling back can destroy data: production needs the same explicit
	// confirmation as dropping. These flags are consumed here, not passed on.
	var f destructiveFlags
	var passthrough []string
	for _, arg := range args {
		switch arg {
		case "--force-production":
			f.forceProduction = true
		case "--yes":
			f.yes = true
		default:
			passthrough = append(passthrough, arg)
		}
	}
	if err := d.confirm(target, "roll back", f); err != nil {
		return err
	}
	return d.delegate(ctx, append([]string{"rollback"}, passthrough...)...)
}

func (d *dbCommand) setup(ctx context.Context) error {
	if err := d.create(ctx); err != nil {
		return err
	}
	if err := d.delegate(ctx, "migrate"); err != nil {
		return err
	}
	return d.delegate(ctx, "seed")
}

func (d *dbCommand) prepare(ctx context.Context) error {
	target, err := d.target()
	if err != nil {
		return err
	}
	exists, err := target.Exists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return d.setup(ctx)
	}
	return d.delegate(ctx, "migrate")
}

func (d *dbCommand) reset(ctx context.Context, f destructiveFlags) error {
	if err := d.drop(ctx, f); err != nil {
		return err
	}
	return d.setup(ctx)
}

func (d *dbCommand) schemaDump(ctx context.Context) error {
	target, err := d.target()
	if err != nil {
		return err
	}
	schema, err := target.DumpSchema(ctx)
	if err != nil {
		return err
	}
	path := filepath.Join(d.root, filepath.FromSlash(dbtasks.SchemaFile))
	if err := writeFileAtomic(path, schema); err != nil {
		return err
	}
	d.say("wrote %s", dbtasks.SchemaFile)
	return nil
}

func (d *dbCommand) schemaLoad(ctx context.Context, f destructiveFlags) error {
	target, err := d.target()
	if err != nil {
		return err
	}
	if err := d.confirm(target, "load a schema into", f); err != nil {
		return err
	}
	path := filepath.Join(d.root, filepath.FromSlash(dbtasks.SchemaFile))
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s not found: run forge db schema:dump first", dbtasks.SchemaFile)
	}
	if err := target.LoadSchema(ctx, path); err != nil {
		return err
	}
	d.say("loaded %s into %s", dbtasks.SchemaFile, target.Describe())
	return nil
}

// console runs the database's own client. Arguments after -- reach it.
func (d *dbCommand) console(ctx context.Context, args []string) error {
	var extra []string
	if len(args) > 0 {
		if args[0] != "--" {
			return errors.New("usage: forge db console [-- CLIENT-ARGUMENTS]")
		}
		extra = args[1:]
	}
	target, err := d.target()
	if err != nil {
		return err
	}
	console, err := target.PrepareConsole(extra)
	if err != nil {
		return err
	}
	defer console.Cleanup()
	command := console.Command(ctx)
	command.Stdin, command.Stdout, command.Stderr = d.stdin, d.stdout, d.stderr
	// Ctrl-C belongs to the client (it cancels a query), not to forge.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("%s exited with status %d", filepath.Base(console.Path), exit.ExitCode())
		}
		return err
	}
	return nil
}

// confirm is the guard in front of every destructive command. Outside
// production there is nothing to confirm. In production it refuses unless
// --force-production is given AND either a person types the database name at
// a terminal or --yes is given with FORGE_ALLOW_DESTRUCTIVE=true in the
// process environment. An env file cannot grant it.
func (d *dbCommand) confirm(target *dbtasks.Target, action string, f destructiveFlags) error {
	if target.Environment != config.Production {
		return nil
	}
	if !f.forceProduction {
		return fmt.Errorf("refusing to %s the production database (%s): pass --force-production, and confirm", action, target.Describe())
	}
	if f.yes {
		if value, _ := d.processEnv("FORGE_ALLOW_DESTRUCTIVE"); value != "true" {
			return errors.New("--yes in production also needs FORGE_ALLOW_DESTRUCTIVE=true in the process environment")
		}
		return nil
	}
	if !d.interactive {
		return errors.New("confirming a destructive command in production needs an interactive terminal, or --yes with FORGE_ALLOW_DESTRUCTIVE=true")
	}
	expected := filepath.Base(target.Name)
	fmt.Fprintf(d.stderr, "This will %s the PRODUCTION %s.\nType the database name (%s) to continue: ", action, target.Describe(), expected)
	line, err := bufio.NewReader(d.stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("no confirmation given")
	}
	if strings.TrimSpace(line) != expected {
		return errors.New("confirmation did not match: nothing was changed")
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".schema-*.sql")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// builtApp is the application binary, built once for a `forge db` run.
type builtApp struct {
	root, dir, binary string
	env               []string
	stdout, stderr    io.Writer
}

func buildAppRunner(ctx context.Context, root string, stdout, stderr io.Writer) (appRunner, error) {
	name, err := resolveCommand(root, nil)
	if err != nil {
		return nil, err
	}
	env, err := developmentEnvironment(root)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "forge-db-")
	if err != nil {
		return nil, err
	}
	app := &builtApp{root: root, dir: dir, binary: filepath.Join(dir, name), env: env, stdout: stdout, stderr: stderr}
	build := goCommand(ctx, root, nil, stdout, stderr, "build", "-o", app.binary, "./cmd/"+name)
	devloop.IsolateCommand(build)
	if err := build.Run(); err != nil {
		app.Close()
		return nil, fmt.Errorf("build: %w", err)
	}
	return app, nil
}

func (a *builtApp) Run(ctx context.Context, args []string) error {
	command := exec.CommandContext(ctx, a.binary, args...) // #nosec G204 -- the binary built above, forge-chosen arguments
	command.Dir, command.Env = a.root, a.env
	command.Stdout, command.Stderr = a.stdout, a.stderr
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("application command %q failed with status %d", args[0], exit.ExitCode())
		}
		return err
	}
	return nil
}

func (a *builtApp) Close() { _ = os.RemoveAll(a.dir) }
