package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sync"
)

var sincePattern = regexp.MustCompile(`^[0-9A-Za-z:.+-]{1,40}$`)

// LogsOptions select what `forge logs` streams.
type LogsOptions struct {
	Role   string
	Host   string
	Follow bool
	Since  string
	Lines  int
	// Grep keeps only lines matching this regular expression (RE2).
	Grep string
}

func (d *Deployer) selectHosts(role, host string) (string, []string, error) {
	if role == "" {
		role = d.Config.PrimaryRole()
	}
	r, ok := d.Config.Servers[role]
	if !ok {
		return "", nil, fmt.Errorf("unknown role %q", role)
	}
	if host == "" {
		return role, r.Hosts, nil
	}
	if !slices.Contains(r.Hosts, host) {
		return "", nil, fmt.Errorf("host %q does not run role %s", host, role)
	}
	return role, []string{host}, nil
}

// Logs streams the logs of the current role container from every selected
// host, each line prefixed with "host|". It returns when every stream has
// ended or ctx is cancelled; cancellation stops the remote `docker logs`.
// Without Follow at most Config.Parallelism hosts are read at once; with
// Follow every host streams at once.
func (d *Deployer) Logs(ctx context.Context, out io.Writer, opts LogsOptions) error {
	if err := d.prepare(); err != nil {
		return err
	}
	role, hosts, err := d.selectHosts(opts.Role, opts.Host)
	if err != nil {
		return err
	}
	if opts.Since != "" && !sincePattern.MatchString(opts.Since) {
		return errors.New("--since must be a duration such as 1h or a timestamp")
	}
	if opts.Lines < 0 || opts.Lines > 100000 {
		return errors.New("--lines must be between 0 and 100000")
	}
	if opts.Lines == 0 && opts.Since == "" {
		opts.Lines = 100
		if opts.Follow {
			opts.Lines = 10
		}
	}
	var grep *regexp.Regexp
	if opts.Grep != "" {
		if grep, err = regexp.Compile(opts.Grep); err != nil {
			return fmt.Errorf("--grep: %w", err)
		}
	}
	shared := &lockedOutput{w: out}
	cmd := d.cmds.logs(role, opts.Follow, opts.Since, opts.Lines)
	streamHost := func(ctx context.Context, host string) error {
		writer := &prefixWriter{prefix: host + "|", out: shared, grep: grep, redactor: d.redactor}
		defer writer.flush()
		return d.stream(ctx, host, cmd, writer)
	}
	if !opts.Follow {
		// Every stream ends, so read at most Parallelism hosts at once, like
		// every other multi-host operation.
		return d.eachHost(ctx, hosts, streamHost)
	}
	// A followed stream never ends on its own: a host waiting for a free slot
	// would never be shown, so every selected host streams at once (narrow
	// them with --role and --host).
	errs := make([]error, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Go(func() { errs[i] = streamHost(ctx, host) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

type lockedOutput struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedOutput) writeLine(line []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(line)
}

// prefixWriter splits output into lines, filters, redacts and prefixes
// them. It is safe for concurrent use (stdout and stderr copiers).
type prefixWriter struct {
	mu       sync.Mutex
	prefix   string
	out      *lockedOutput
	grep     *regexp.Regexp
	redactor *Redactor
	pending  []byte
}

func (p *prefixWriter) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = append(p.pending, data...)
	for {
		i := bytes.IndexByte(p.pending, '\n')
		if i < 0 {
			break
		}
		p.emit(p.pending[:i])
		p.pending = p.pending[i+1:]
	}
	if len(p.pending) > 64<<10 {
		p.emit(p.pending)
		p.pending = nil
	}
	return len(data), nil
}

func (p *prefixWriter) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) > 0 {
		p.emit(p.pending)
		p.pending = nil
	}
}

func (p *prefixWriter) emit(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if p.grep != nil && !p.grep.Match(line) {
		return
	}
	text := p.redactor.Redact(string(line))
	p.out.writeLine([]byte(p.prefix + text + "\n"))
}

// ExecOptions describe `forge app exec`.
type ExecOptions struct {
	Role string
	Host string
	// Interactive allocates a terminal (ssh -t, docker -it).
	Interactive bool
	// Reuse runs inside the running container instead of a new one-off
	// container of the same image.
	Reuse bool
	Argv  []string
}

// Exec runs a command on one host of the role (the first unless Host is
// set). By default it starts a one-off container of the image the role
// currently runs, with the role's env file, so the arguments go to the
// image entrypoint: `forge app exec migrate status` runs
// `/app/server migrate status`.
func (d *Deployer) Exec(ctx context.Context, out io.Writer, opts ExecOptions) error {
	if err := d.prepare(); err != nil {
		return err
	}
	if len(opts.Argv) == 0 {
		return errors.New("a command is required")
	}
	for _, arg := range opts.Argv {
		if !argPattern.MatchString(arg) {
			return errors.New("command arguments must not contain control characters")
		}
	}
	role, hosts, err := d.selectHosts(opts.Role, opts.Host)
	if err != nil {
		return err
	}
	host := hosts[0]
	script := d.cmds.execOneOff(role, opts.Argv, opts.Interactive)
	if opts.Reuse {
		script = d.cmds.execReuse(role, opts.Argv, opts.Interactive)
	}
	if opts.Interactive {
		return d.interactive(ctx, host, script)
	}
	writer := &prefixWriter{out: &lockedOutput{w: out}, redactor: d.redactor}
	err = d.stream(ctx, host, Command{Script: script}, writer)
	writer.flush()
	return err
}

// Shell opens an interactive shell on one host of the role. Distroless
// images have no shell: pass a debug image (for example busybox), which
// runs as a sidecar in the container's network and process namespaces
// without the application's environment.
func (d *Deployer) Shell(ctx context.Context, role, host, image string) error {
	if err := d.prepare(); err != nil {
		return err
	}
	role, hosts, err := d.selectHosts(role, host)
	if err != nil {
		return err
	}
	script := d.cmds.execReuse(role, []string{"sh"}, true)
	if image != "" {
		if !imageRefPattern.MatchString(image) {
			return fmt.Errorf("--image %q is not an image reference", image)
		}
		script = d.cmds.debugShell(role, image)
	}
	return d.interactive(ctx, hosts[0], script)
}
