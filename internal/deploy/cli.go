package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type commonFlags struct {
	config  string
	secrets string
	dryRun  bool
	json    bool
}

func newFlags(name string, stderr io.Writer, withJSON bool) (*flag.FlagSet, *commonFlags) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	common := &commonFlags{}
	flags.StringVar(&common.config, "config", DefaultConfigPath, "deploy configuration")
	flags.StringVar(&common.config, "c", DefaultConfigPath, "shorthand for --config")
	flags.StringVar(&common.secrets, "secrets", DefaultSecretsPath, "secrets file")
	flags.BoolVar(&common.dryRun, "dry-run", false, "print the commands (secrets redacted) instead of running them")
	if withJSON {
		flags.BoolVar(&common.json, "json", false, "print a schema-versioned JSON report on stdout")
	}
	return flags, common
}

// parseAll parses flags anywhere among the arguments and returns the
// positional ones.
func parseAll(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		if flags.NArg() == 0 {
			return positional, nil
		}
		rest := flags.Args()
		if len(args) > 0 && len(rest) < len(args) && args[len(args)-len(rest)-1] == "--" {
			return append(positional, rest...), nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func (c *commonFlags) deployer(stdout, stderr io.Writer) (*Deployer, error) {
	cfg, err := LoadConfig(c.config)
	if err != nil {
		return nil, err
	}
	secrets, err := LoadSecrets(c.secrets, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	log := stderr
	if c.dryRun {
		log = stdout
	}
	return &Deployer{
		Config: cfg, Secrets: secrets,
		Runner: NewSSHRunner(cfg.SSH),
		Local:  ExecLocal{Stdout: stderr, Stderr: stderr},
		Log:    log, DryRun: c.dryRun,
	}, nil
}

func closeRunner(d *Deployer) {
	if d != nil && d.Runner != nil {
		_ = d.Runner.Close()
	}
}

// RunDeploy implements `forge deploy [SUBCOMMAND]`.
func RunDeploy(args []string, stdout, stderr io.Writer) error {
	ctx, stop := signalContext()
	defer stop()
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "":
		return runDeployCommand(ctx, args, stdout, stderr)
	case "init":
		return runInit(args, stdout, stderr)
	case "setup", "prune":
		return runSimple(ctx, sub, args, stdout, stderr)
	case "rollback":
		return runRollback(ctx, args, stdout, stderr)
	case "details":
		return runDetails(ctx, args, stdout, stderr)
	case "lock":
		return runLock(ctx, args, stdout, stderr)
	case "maintenance":
		return runMaintenance(ctx, args, stdout, stderr)
	default:
		return fmt.Errorf("unknown deploy command %q (setup, rollback, prune, details, lock, maintenance, init)", sub)
	}
}

func runDeployCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, common := newFlags("deploy", stderr, true)
	version := flags.String("version", "", "image tag to deploy (default: abbreviated git commit)")
	skipBuild := flags.Bool("skip-build", false, "deploy an image already pushed to the registry")
	positional, err := parseAll(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("usage: forge deploy [--version V] [--skip-build] [--dry-run] [--json]")
	}
	d, err := common.deployer(stdout, stderr)
	if err != nil {
		return err
	}
	defer closeRunner(d)
	resolved, err := ResolveVersion(ctx, d.Local, ".", *version)
	if err != nil {
		return err
	}
	report, err := d.Deploy(ctx, DeployOptions{Version: resolved, SkipBuild: *skipBuild})
	return finishReport(common, stdout, report, err, fmt.Sprintf("deployed %s", resolved))
}

func runSimple(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	flags, common := newFlags(name, stderr, true)
	positional, err := parseAll(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("usage: forge deploy %s [--dry-run] [--json]", name)
	}
	d, err := common.deployer(stdout, stderr)
	if err != nil {
		return err
	}
	defer closeRunner(d)
	var report *Report
	if name == "setup" {
		report, err = d.Setup(ctx)
	} else {
		report, err = d.Prune(ctx)
	}
	return finishReport(common, stdout, report, err, name+" complete")
}

func runRollback(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, common := newFlags("rollback", stderr, true)
	positional, err := parseAll(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: forge deploy rollback VERSION [--dry-run] [--json]")
	}
	d, err := common.deployer(stdout, stderr)
	if err != nil {
		return err
	}
	defer closeRunner(d)
	report, err := d.Rollback(ctx, positional[0])
	return finishReport(common, stdout, report, err, "rolled back to "+positional[0])
}

// finishReport prints the report (always in --json mode, also on failure)
// and passes err through.
func finishReport(common *commonFlags, stdout io.Writer, report *Report, err error, summary string) error {
	if common.json && report != nil {
		type output struct {
			*Report
			OK       bool   `json:"ok"`
			Error    string `json:"error,omitempty"`
			ExitCode int    `json:"exit_code"`
		}
		out := output{Report: report, OK: err == nil, ExitCode: ExitCodeOf(err)}
		if err != nil {
			out.Error = err.Error()
		}
		if encodeErr := json.NewEncoder(stdout).Encode(out); encodeErr != nil && err == nil {
			return encodeErr
		}
		return err
	}
	if err != nil {
		return err
	}
	if report != nil {
		for _, warning := range report.Warnings {
			fmt.Fprintf(stdout, "warning: %s\n", warning)
		}
		if common.dryRun {
			summary = "dry run: nothing was changed"
		}
		fmt.Fprintf(stdout, "%s (%s)\n", summary, (time.Duration(report.DurationMS) * time.Millisecond).String())
	}
	return nil
}

func runDetails(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, common := newFlags("details", stderr, true)
	positional, err := parseAll(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 0 || common.dryRun {
		return errors.New("usage: forge deploy details [--json]")
	}
	d, err := common.deployer(stdout, stderr)
	if err != nil {
		return err
	}
	defer closeRunner(d)
	details, err := d.Details(ctx)
	if common.json {
		if encodeErr := json.NewEncoder(stdout).Encode(map[string]any{"schema_version": 1, "service": d.Config.Service, "hosts": details}); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	for _, host := range details {
		fmt.Fprintf(stdout, "%s (%s)\n", host.Host, strings.Join(host.Roles, ", "))
		if host.Error != "" {
			fmt.Fprintf(stdout, "  error: %s\n", host.Error)
		}
		for _, c := range host.Containers {
			fmt.Fprintf(stdout, "  %-8s %-10s %-20s %s\n", c.Role, c.State, c.Version, c.Name)
		}
		for _, service := range host.Proxy {
			var status struct {
				Service, Target string
				Hosts           []string
				TLS             bool
				Maintenance     bool
			}
			if json.Unmarshal(service, &status) == nil {
				fmt.Fprintf(stdout, "  proxy    %s -> %s hosts=%s tls=%v maintenance=%v\n", status.Service, status.Target, strings.Join(status.Hosts, ","), status.TLS, status.Maintenance)
			}
		}
		if host.Lock != nil {
			fmt.Fprintf(stdout, "  lock     held by %s since %s (%s)\n", host.Lock.Holder, host.Lock.AcquiredAt.Format(time.RFC3339), host.Lock.Message)
		}
		for _, entry := range host.Deploys {
			fmt.Fprintf(stdout, "  history  %s\n", entry)
		}
	}
	return err
}

func runLock(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, common := newFlags("lock", stderr, true)
	message := flags.String("message", "manual lock", "reason recorded with the lock")
	force := flags.Bool("force", false, "release a lock held by someone else")
	positional, err := parseAll(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: forge deploy lock status|acquire|release [--message M] [--force]")
	}
	d, err := common.deployer(stdout, stderr)
	if err != nil {
		return err
	}
	defer closeRunner(d)
	switch positional[0] {
	case "status":
		locks, err := d.LockStatus(ctx)
		if err != nil {
			return err
		}
		if common.json {
			return json.NewEncoder(stdout).Encode(map[string]any{"schema_version": 1, "locked": len(locks) > 0, "locks": locks})
		}
		if len(locks) == 0 {
			fmt.Fprintln(stdout, "not locked")
		}
		for _, host := range sortedKeys(locks) {
			info := locks[host]
			fmt.Fprintf(stdout, "%s: held by %s since %s (%s)\n", host, info.Holder, info.AcquiredAt.Format(time.RFC3339), info.Message)
		}
		return nil
	case "acquire":
		if err := d.LockAcquire(ctx, *message); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "lock acquired")
		return nil
	case "release":
		if err := d.LockRelease(ctx, *force); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "lock released")
		return nil
	default:
		return errors.New("usage: forge deploy lock status|acquire|release")
	}
}

func runMaintenance(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, common := newFlags("maintenance", stderr, false)
	positional, err := parseAll(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 || (positional[0] != "on" && positional[0] != "off") {
		return errors.New("usage: forge deploy maintenance on|off [--dry-run]")
	}
	d, err := common.deployer(stdout, stderr)
	if err != nil {
		return err
	}
	defer closeRunner(d)
	return d.Maintenance(ctx, positional[0] == "on")
}

var invalidServiceChars = regexp.MustCompile(`[^a-z0-9-]+`)

// defaultServiceName derives the service from go.mod's module path or the
// directory name.
func defaultServiceName(root string) string {
	name := ""
	if content, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		for line := range strings.Lines(string(content)) {
			if module, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
				name = module[strings.LastIndex(module, "/")+1:]
				break
			}
		}
	}
	if name == "" {
		if abs, err := filepath.Abs(root); err == nil {
			name = filepath.Base(abs)
		}
	}
	name = strings.Trim(invalidServiceChars.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(name) > 40 {
		name = name[:40]
	}
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "app-" + name
	}
	return strings.TrimRight(name, "-")
}

func runInit(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	service := flags.String("service", "", "service name (default: from go.mod)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: forge deploy init [--service NAME]")
	}
	if *service == "" {
		*service = defaultServiceName(".")
	}
	changed, err := Init(".", *service)
	for _, path := range changed {
		fmt.Fprintf(stdout, "wrote %s\n", path)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "next: edit config/deploy.yml and .forge/secrets, then run forge deploy setup")
	return nil
}

// RunLogs implements `forge logs`.
func RunLogs(args []string, stdout, stderr io.Writer) error {
	ctx, stop := signalContext()
	defer stop()
	flags, common := newFlags("logs", stderr, false)
	var opts LogsOptions
	flags.BoolVar(&opts.Follow, "follow", false, "keep streaming new lines")
	flags.BoolVar(&opts.Follow, "f", false, "shorthand for --follow")
	flags.StringVar(&opts.Role, "role", "", "role (default: the primary role)")
	flags.StringVar(&opts.Host, "host", "", "only this host")
	flags.StringVar(&opts.Since, "since", "", "only lines since this duration (1h) or timestamp")
	flags.StringVar(&opts.Grep, "grep", "", "only lines matching this regular expression")
	flags.IntVar(&opts.Lines, "lines", 0, "lines per host to start with (default 100, 10 with --follow)")
	positional, err := parseAll(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("usage: forge logs [-f] [--role R] [--host H] [--since 1h] [--grep X] [--lines N]")
	}
	d, err := common.deployer(stdout, stderr)
	if err != nil {
		return err
	}
	defer closeRunner(d)
	return d.Logs(ctx, stdout, opts)
}

// RunApp implements `forge app exec|shell`.
func RunApp(args []string, stdout, stderr io.Writer) error {
	ctx, stop := signalContext()
	defer stop()
	if len(args) == 0 || (args[0] != "exec" && args[0] != "shell") {
		return errors.New("usage: forge app exec [--role R] [--host H] [--interactive] [--reuse] CMD... | forge app shell [--image IMAGE]")
	}
	sub := args[0]
	flags, common := newFlags("app "+sub, stderr, false)
	var opts ExecOptions
	flags.StringVar(&opts.Role, "role", "", "role (default: the primary role)")
	flags.StringVar(&opts.Host, "host", "", "host (default: the first host of the role)")
	image := ""
	if sub == "exec" {
		flags.BoolVar(&opts.Interactive, "interactive", false, "attach a terminal (ssh -t, docker -it)")
		flags.BoolVar(&opts.Interactive, "i", false, "shorthand for --interactive")
		flags.BoolVar(&opts.Reuse, "reuse", false, "run inside the running container instead of a one-off container")
	} else {
		flags.StringVar(&image, "image", "", "debug image with a shell, run as a sidecar (distroless images have no shell)")
	}
	// Flags must precede the command, whose own flags are passed through.
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	d, err := common.deployer(stdout, stderr)
	if err != nil {
		return err
	}
	defer closeRunner(d)
	if sub == "shell" {
		if flags.NArg() != 0 {
			return errors.New("usage: forge app shell [--role R] [--host H] [--image IMAGE]")
		}
		return d.Shell(ctx, opts.Role, opts.Host, image)
	}
	opts.Argv = flags.Args()
	return d.Exec(ctx, stdout, opts)
}
