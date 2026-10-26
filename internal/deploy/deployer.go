package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

var versionPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// Deployer runs deploy operations for one configuration.
type Deployer struct {
	Config  *Config
	Secrets Secrets
	Runner  Runner
	Local   LocalRunner
	// Root is the project directory (hooks, build context).
	Root string
	// Log receives progress lines; secrets are redacted.
	Log io.Writer
	// Performer identifies who deploys (user@machine).
	Performer string
	Now       func() time.Time
	// Suffix makes container names unique; tests make it deterministic.
	Suffix func() string
	// DryRun prints mutating commands instead of running them; read-only
	// queries still run so the plan names real containers.
	DryRun bool

	cmds     commands
	redactor *Redactor
	logMu    sync.Mutex
	prepared bool
}

// Prepare validates secrets and wires defaults. It is called by every
// operation.
func (d *Deployer) prepare() error {
	if d.prepared {
		return nil
	}
	if err := d.Secrets.Require(d.Config.SecretNames()); err != nil {
		return err
	}
	d.cmds = commands{cfg: d.Config}
	d.redactor = d.Secrets.Redactor()
	if d.Log == nil {
		d.Log = io.Discard
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Suffix == nil {
		d.Suffix = randomSuffix
	}
	if d.Performer == "" {
		d.Performer = Performer()
	}
	if d.Root == "" {
		d.Root = "."
	}
	if d.DryRun {
		d.Runner = &dryRunner{inner: d.Runner, out: d.Log, redactor: d.redactor}
		d.Local = &dryLocal{inner: d.Local, out: d.Log, redactor: d.redactor}
	}
	d.prepared = true
	return nil
}

func randomSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Performer returns user@hostname of the local user.
func Performer() string {
	name := "unknown"
	if current, err := user.Current(); err == nil {
		name = current.Username
	}
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return name + "@" + host
}

func (d *Deployer) logf(host, format string, args ...any) {
	line := d.redactor.Redact(fmt.Sprintf(format, args...))
	d.logMu.Lock()
	defer d.logMu.Unlock()
	if d.DryRun {
		fmt.Fprintf(d.Log, "%-15s # %s\n", host, line)
		return
	}
	fmt.Fprintf(d.Log, "%-15s %s\n", host, line)
}

// run, stream and interactive are the only ways operations reach the
// Runner: each redacts secrets from the error it returns, because remote
// stderr and ssh diagnostics end up in `forge: <error>` output.
func (d *Deployer) run(ctx context.Context, host string, cmd Command) (Result, error) {
	result, err := d.Runner.Run(ctx, host, cmd)
	return result, d.redactError(err)
}

func (d *Deployer) stream(ctx context.Context, host string, cmd Command, out io.Writer) error {
	return d.redactError(d.Runner.Stream(ctx, host, cmd, out))
}

func (d *Deployer) interactive(ctx context.Context, host, script string) error {
	return d.redactError(d.Runner.Interactive(ctx, host, script))
}

func (d *Deployer) redactError(err error) error {
	if err == nil {
		return nil
	}
	// A bare *RemoteError keeps its type (exit codes are read from it);
	// its Error text is built from Host and Stderr only.
	if remote, ok := err.(*RemoteError); ok { //nolint:errorlint // only a bare *RemoteError keeps its type; a wrapped one is flattened to redacted text on purpose
		remote.Stderr = d.redactor.Redact(remote.Stderr)
		return remote
	}
	return errors.New(d.redactor.Redact(err.Error()))
}

// eachHost runs fn for every host with bounded parallelism (sequentially in
// dry-run, so the plan is printed in a stable order) and joins the errors.
func (d *Deployer) eachHost(ctx context.Context, hosts []string, fn func(context.Context, string) error) error {
	limit := d.Config.Parallelism
	if d.DryRun || limit < 2 {
		var errs []error
		for _, host := range hosts {
			errs = append(errs, fn(ctx, host))
		}
		return errors.Join(errs...)
	}
	errs := make([]error, len(hosts))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[i] = fn(ctx, host)
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// ResolveVersion returns explicit when set, else the abbreviated commit of
// HEAD. A working tree with uncommitted changes needs an explicit version:
// the image would not match any commit.
func ResolveVersion(ctx context.Context, local LocalRunner, root, explicit string) (string, error) {
	if explicit != "" {
		if !versionPattern.MatchString(explicit) {
			return "", fmt.Errorf("version %q must match %s", explicit, versionPattern)
		}
		return explicit, nil
	}
	head, err := local.Output(ctx, root, []string{"git", "rev-parse", "--short=12", "--verify", "HEAD"})
	if err != nil {
		return "", fmt.Errorf("determine version from git (or pass --version): %w", err)
	}
	status, err := local.Output(ctx, root, []string{"git", "status", "--porcelain", "--untracked-files=no"})
	if err != nil {
		return "", fmt.Errorf("check working tree: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return "", errors.New("the working tree has uncommitted changes; commit them or pass --version")
	}
	return strings.TrimSpace(head), nil
}

// Container is one application container found on a host.
type Container struct {
	Name    string    `json:"name"`
	Role    string    `json:"role"`
	Version string    `json:"version"`
	State   string    `json:"state"`
	Image   string    `json:"image"`
	Created time.Time `json:"created"`
}

// Running reports whether the container is running or restarting.
func (c Container) Running() bool { return c.State == "running" || c.State == "restarting" }

type psLine struct {
	Names     string
	Labels    string
	State     string
	Image     string
	CreatedAt string
}

// parseInventory reads `docker ps --format '{{json .}}'` output, newest
// first.
func parseInventory(output string) ([]Container, error) {
	var containers []Container
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ps psLine
		if err := json.Unmarshal([]byte(line), &ps); err != nil {
			return nil, fmt.Errorf("unexpected docker ps output: %w", err)
		}
		labels := map[string]string{}
		for pair := range strings.SplitSeq(ps.Labels, ",") {
			if key, value, ok := strings.Cut(pair, "="); ok {
				labels[key] = value
			}
		}
		created, _ := time.Parse("2006-01-02 15:04:05 -0700 MST", ps.CreatedAt)
		containers = append(containers, Container{
			Name: ps.Names, Role: labels[labelRole], Version: labels[labelVersion],
			State: ps.State, Image: ps.Image, Created: created,
		})
	}
	slices.SortStableFunc(containers, func(a, b Container) int { return b.Created.Compare(a.Created) })
	return containers, nil
}

func (d *Deployer) inventory(ctx context.Context, host string) ([]Container, error) {
	result, err := d.run(ctx, host, d.cmds.inventory())
	if err != nil {
		return nil, err
	}
	return parseInventory(result.Stdout)
}

func (d *Deployer) inventories(ctx context.Context, hosts []string) (map[string][]Container, error) {
	var mu sync.Mutex
	all := map[string][]Container{}
	err := d.eachHost(ctx, hosts, func(ctx context.Context, host string) error {
		containers, err := d.inventory(ctx, host)
		mu.Lock()
		all[host] = containers
		mu.Unlock()
		return err
	})
	return all, err
}

// LockInfo is the record stored in the lock directory on each host.
type LockInfo struct {
	Holder     string    `json:"holder"`
	Message    string    `json:"message"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// parseLockInfo reads the stdout of the lock commands. An empty record or
// {} is a lock whose acquire was interrupted before it wrote the record.
func parseLockInfo(stdout string) LockInfo {
	text := strings.TrimSpace(stdout)
	if text == "" || text == "{}" {
		return LockInfo{Holder: "unknown", Message: "lock without a record: an acquire was interrupted; a deploy takes it over once it is a minute old if its info file is missing, otherwise use forge deploy lock release --force"}
	}
	var info LockInfo
	if json.Unmarshal([]byte(text), &info) != nil {
		return LockInfo{Holder: "unknown", Message: "unreadable lock record"}
	}
	return info
}

func (d *Deployer) acquire(ctx context.Context, hosts []string, message string) (func(), error) {
	record, err := json.Marshal(LockInfo{Holder: d.Performer, Message: message, AcquiredAt: d.Now().UTC()})
	if err != nil {
		return nil, err
	}
	record = append(record, '\n')
	var mu sync.Mutex
	var acquired []string
	held := map[string]LockInfo{}
	err = d.eachHost(ctx, hosts, func(ctx context.Context, host string) error {
		result, err := d.run(ctx, host, d.cmds.acquireLock(record))
		mu.Lock()
		defer mu.Unlock()
		var remote *RemoteError
		switch {
		case err == nil:
			acquired = append(acquired, host)
			if strings.Contains(result.Stderr, staleLockNotice) {
				d.logf("warning", "%s: %s", host, staleLockNotice)
			}
		case errors.As(err, &remote) && remote.ExitCode == lockHeldExitStatus:
			held[host] = parseLockInfo(result.Stdout)
			return nil
		}
		return err
	})
	release := func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := d.eachHost(releaseCtx, acquired, func(ctx context.Context, host string) error {
			_, err := d.run(ctx, host, d.cmds.releaseLock(record))
			var remote *RemoteError
			switch {
			case errors.As(err, &remote) && remote.ExitCode == lockGoneExitStatus:
				d.logf("warning", "%s: the deploy lock was already released (by --force?)", host)
				return nil
			case errors.As(err, &remote) && remote.ExitCode == lockNotOursExitStatus:
				// Somebody force-released and took the lock: not ours to remove.
				d.logf("warning", "%s: the deploy lock is now held by someone else and was left in place", host)
				return nil
			}
			return err
		}); err != nil {
			d.logf("warning", "releasing the deploy lock failed: %v", err)
		}
	}
	if len(held) > 0 || err != nil {
		release()
		if len(held) > 0 {
			return nil, withCode(ExitLockHeld, &LockHeldError{Holders: held})
		}
		return nil, err
	}
	for _, host := range hosts {
		d.logf(host, "lock acquired (%s)", message)
	}
	return release, nil
}

// LockAcquire takes the deploy lock on every host until LockRelease.
func (d *Deployer) LockAcquire(ctx context.Context, message string) error {
	if err := d.prepare(); err != nil {
		return err
	}
	_, err := d.acquire(ctx, d.Config.AllHosts(), message)
	return err
}

// lockRecords returns the raw record per host; hosts without a lock are
// absent, and a lock without a record is "{}".
func (d *Deployer) lockRecords(ctx context.Context) (map[string]string, error) {
	var mu sync.Mutex
	records := map[string]string{}
	err := d.eachHost(ctx, d.Config.AllHosts(), func(ctx context.Context, host string) error {
		result, err := d.run(ctx, host, d.cmds.lockStatus())
		if err != nil {
			return err
		}
		if strings.TrimSpace(result.Stdout) == "" {
			return nil
		}
		mu.Lock()
		records[host] = strings.TrimSpace(result.Stdout)
		mu.Unlock()
		return nil
	})
	return records, err
}

// LockStatus returns the lock record per host; hosts without a lock are
// absent. A lock left without a record reports holder "unknown".
func (d *Deployer) LockStatus(ctx context.Context) (map[string]LockInfo, error) {
	if err := d.prepare(); err != nil {
		return nil, err
	}
	records, err := d.lockRecords(ctx)
	locks := map[string]LockInfo{}
	for host, record := range records {
		locks[host] = parseLockInfo(record)
	}
	return locks, err
}

// LockRelease removes the deploy lock. Without force it removes only locks
// held by this performer, and only while the record is unchanged; it
// refuses one held by someone else (or without a record).
func (d *Deployer) LockRelease(ctx context.Context, force bool) error {
	if err := d.prepare(); err != nil {
		return err
	}
	if force {
		return d.eachHost(ctx, d.Config.AllHosts(), func(ctx context.Context, host string) error {
			_, err := d.run(ctx, host, d.cmds.forceReleaseLock())
			if err == nil {
				d.logf(host, "lock released")
			}
			return err
		})
	}
	records, err := d.lockRecords(ctx)
	if err != nil {
		return err
	}
	for _, host := range sortedKeys(records) {
		if info := parseLockInfo(records[host]); records[host] == "{}" || info.Holder != d.Performer {
			return withCode(ExitLockHeld, fmt.Errorf("%s: lock is held by %s (%s); pass --force to release it anyway", host, info.Holder, info.Message))
		}
	}
	return d.eachHost(ctx, sortedKeys(records), func(ctx context.Context, host string) error {
		_, err := d.run(ctx, host, d.cmds.releaseLock([]byte(records[host]+"\n")))
		var remote *RemoteError
		if errors.As(err, &remote) && remote.ExitCode == lockGoneExitStatus {
			d.logf(host, "lock was already released")
			return nil
		}
		if errors.As(err, &remote) && remote.ExitCode == lockNotOursExitStatus {
			return withCode(ExitLockHeld, fmt.Errorf("%s: the lock changed while releasing it; left in place (pass --force to release it anyway)", host))
		}
		if err == nil {
			d.logf(host, "lock released")
		}
		return err
	})
}

// hook runs .forge/hooks/NAME when it exists. A non-zero exit aborts.
func (d *Deployer) hook(ctx context.Context, name string, env map[string]string) error {
	path := filepath.Join(d.Root, d.Config.Hooks, name)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return withCode(ExitHookFailed, fmt.Errorf("hook %s: %w", name, err))
	}
	if info.IsDir() || info.Mode().Perm()&0o100 == 0 {
		return withCode(ExitHookFailed, fmt.Errorf("hook %s exists but is not executable (chmod +x %s)", name, path))
	}
	var vars []string
	for _, key := range sortedKeys(env) {
		vars = append(vars, key+"="+env[key])
	}
	d.logf("local", "hook %s", name)
	if err := d.redactError(d.Local.Run(ctx, d.Root, []string{path}, nil, vars)); err != nil {
		return withCode(ExitHookFailed, fmt.Errorf("hook %s failed: %w", name, err))
	}
	return nil
}

func (d *Deployer) hookEnv(command, version string, started time.Time) map[string]string {
	env := map[string]string{
		"FORGE_COMMAND":   command,
		"FORGE_SERVICE":   d.Config.Service,
		"FORGE_VERSION":   version,
		"FORGE_IMAGE":     d.cmds.imageRef(version),
		"FORGE_HOSTS":     strings.Join(d.Config.AppHosts(), ","),
		"FORGE_ROLES":     strings.Join(d.Config.Roles(), ","),
		"FORGE_PERFORMER": d.Performer,
	}
	if !started.IsZero() {
		env["FORGE_RUNTIME"] = fmt.Sprint(int(d.Now().Sub(started).Seconds()))
	}
	return env
}

func (d *Deployer) build(ctx context.Context, version string) error {
	cfg := d.Config
	if cfg.Registry.Username != "" {
		password, _ := d.Secrets.Get(cfg.Registry.Password)
		argv := []string{"docker", "login"}
		if cfg.Registry.Server != "" {
			argv = append(argv, cfg.Registry.Server)
		}
		argv = append(argv, "--username", cfg.Registry.Username, "--password-stdin")
		if err := d.redactError(d.Local.Run(ctx, d.Root, argv, []byte(password), nil)); err != nil {
			return fmt.Errorf("registry login: %w", err)
		}
	}
	argv := []string{"docker", "build", "--tag", d.cmds.imageRef(version), "--file", cfg.Builder.Dockerfile,
		"--label", labelService + "=" + cfg.Service, "--label", labelVersion + "=" + version}
	if cfg.Builder.Platform != "" {
		argv = append(argv, "--platform", cfg.Builder.Platform)
	}
	for _, key := range sortedKeys(cfg.Builder.Args) {
		argv = append(argv, "--build-arg", key+"="+cfg.Builder.Args[key])
	}
	argv = append(argv, cfg.Builder.Context)
	d.logf("local", "building %s", d.cmds.imageRef(version))
	if err := d.redactError(d.Local.Run(ctx, d.Root, argv, nil, nil)); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if err := d.redactError(d.Local.Run(ctx, d.Root, []string{"docker", "push", d.cmds.imageRef(version)}, nil, nil)); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}
