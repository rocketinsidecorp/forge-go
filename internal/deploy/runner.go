package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Command is one remote shell command. Script is a POSIX sh command line
// built only from validated configuration and quoted values; secrets never
// appear in it. Secret material travels on Stdin, which is never printed.
type Command struct {
	Script string
	// Stdin is written to the command's standard input.
	Stdin []byte
	// StdinNote describes Stdin for --dry-run output without revealing it.
	StdinNote string
	// ReadOnly commands only inspect state; --dry-run executes them so the
	// printed plan names the real containers it would touch.
	ReadOnly bool
}

// Result is the captured output of a command.
type Result struct {
	Stdout string
	Stderr string
}

// RemoteError is returned when a remote command exits non-zero.
type RemoteError struct {
	Host     string
	ExitCode int
	Stderr   string
}

func (e *RemoteError) Error() string {
	message := strings.TrimSpace(e.Stderr)
	if len(message) > 2000 {
		message = message[:2000] + "..."
	}
	if message == "" {
		return fmt.Sprintf("%s: remote command exited with status %d", e.Host, e.ExitCode)
	}
	return fmt.Sprintf("%s: remote command exited with status %d: %s", e.Host, e.ExitCode, message)
}

// Runner executes commands on hosts.
type Runner interface {
	// Run executes cmd on host and captures its output. A non-zero exit is
	// a *RemoteError.
	Run(ctx context.Context, host string, cmd Command) (Result, error)
	// Stream executes cmd and copies its combined output to out until it
	// exits or ctx ends. On cancellation the command's stdin is closed
	// first, which lets watchdog scripts stop remote processes.
	Stream(ctx context.Context, host string, cmd Command, out io.Writer) error
	// Interactive executes script attached to the local terminal.
	Interactive(ctx context.Context, host, script string) error
	// Close releases connections.
	Close() error
}

// Quote returns s as a single POSIX shell word.
func Quote(s string) string {
	if s != "" && safeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// Join quotes and joins argv into a command line.
func Join(argv ...string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = Quote(arg)
	}
	return strings.Join(quoted, " ")
}

// SSHRunner runs commands with the system ssh client, so ~/.ssh/config,
// the agent, known_hosts and ProxyJump work as usual. Host keys are always
// verified (StrictHostKeyChecking is never relaxed) and one multiplexed
// master connection per host is shared by all commands of an invocation.
type SSHRunner struct {
	User      string
	Port      int
	ProxyJump string
	// ConfigFile, when set, is passed as ssh -F (tests use it).
	ConfigFile string
	// Binary defaults to "ssh".
	Binary string
	// ConnectTimeout defaults to 10s.
	ConnectTimeout time.Duration

	once       sync.Once
	controlDir string
	initErr    error
	mu         sync.Mutex
	hosts      map[string]bool
}

// NewSSHRunner returns a runner configured from cfg.SSH.
func NewSSHRunner(cfg SSH) *SSHRunner {
	return &SSHRunner{User: cfg.User, Port: cfg.Port, ProxyJump: cfg.ProxyJump}
}

const (
	// maxControlPath is the longest ControlPath ssh can bind: a Unix socket
	// path must fit sun_path, 104 bytes with its terminator on macOS and the
	// BSDs (108 on Linux), and ssh refuses a longer one on every command with
	// "ControlPath too long".
	maxControlPath = 103
	// controlSocketSuffix is what ssh adds to the directory: "/", the 40
	// hexadecimal characters of %C, and the "." and 16 random characters of
	// the temporary name it binds before renaming the socket.
	controlSocketSuffix = 1 + 40 + 1 + 16
	// controlDirectoryName is the longest name os.MkdirTemp gives the
	// directory: the prefix and a uint32 in decimal.
	controlDirectoryName = len("/forge-ssh-") + len("4294967295")
)

// controlDirectory creates the private (0700) directory that holds the SSH
// control sockets of one run. It lives under $TMPDIR when the sockets fit
// there, and under /tmp when they would not: macOS's default $TMPDIR
// (/var/folders/xx/.../T/, about 49 bytes) is already too long for ssh. It
// fails, naming TMPDIR, when no directory is short enough.
func controlDirectory() (string, error) {
	return makeControlDirectory(os.TempDir(), "/tmp", runtime.GOOS != "windows")
}

// makeControlDirectory is controlDirectory with its choices as arguments: the
// preferred base, the short base to use instead, and whether the sockets are
// Unix sockets (not on Windows, where ssh has no control master).
func makeControlDirectory(base, short string, unixSockets bool) (string, error) {
	if unixSockets && len(base)+controlDirectoryName+controlSocketSuffix > maxControlPath {
		base = short
	}
	dir, err := os.MkdirTemp(base, "forge-ssh-")
	if err != nil {
		return "", fmt.Errorf("create ssh control directory: %w (set TMPDIR to a short, private directory)", err)
	}
	if length := len(dir) + controlSocketSuffix; unixSockets && length > maxControlPath {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("create ssh control directory: the control socket path under %s would be %d bytes, over the %d a Unix socket allows (set TMPDIR to a shorter directory)", dir, length, maxControlPath)
	}
	return dir, nil
}

func (r *SSHRunner) init() error {
	r.once.Do(func() {
		dir, err := controlDirectory()
		if err != nil {
			r.initErr = err
			return
		}
		r.controlDir = dir
		r.hosts = map[string]bool{}
	})
	return r.initErr
}

func (r *SSHRunner) args(host string, interactive bool) []string {
	timeout := r.ConnectTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	args := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + r.controlDir + "/%C",
		"-o", "ControlPersist=60",
		"-o", fmt.Sprintf("ConnectTimeout=%d", int(timeout.Seconds())),
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
	if interactive {
		args = append(args, "-t")
	} else {
		args = append(args, "-o", "BatchMode=yes", "-T")
	}
	if r.ConfigFile != "" {
		args = append(args, "-F", r.ConfigFile)
	}
	if r.User != "" {
		args = append(args, "-l", r.User)
	}
	if r.Port != 0 {
		args = append(args, "-p", fmt.Sprint(r.Port))
	}
	if r.ProxyJump != "" {
		args = append(args, "-J", r.ProxyJump)
	}
	return append(args, "--", host)
}

func (r *SSHRunner) command(ctx context.Context, host, script string, interactive bool) (*exec.Cmd, error) {
	if err := r.init(); err != nil {
		return nil, err
	}
	if !validSSHHost(host) {
		return nil, fmt.Errorf("invalid host %q", host)
	}
	r.mu.Lock()
	r.hosts[host] = true
	r.mu.Unlock()
	binary := r.Binary
	if binary == "" {
		binary = "ssh"
	}
	// The script runs under sh -c regardless of the login shell.
	args := append(r.args(host, interactive), "sh -c "+Quote(script))
	cmd := exec.CommandContext(ctx, binary, args...) // #nosec G204 -- fixed binary; host validated; script quoted.
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}

// Run implements [Runner].
func (r *SSHRunner) Run(ctx context.Context, host string, c Command) (Result, error) {
	cmd, err := r.command(ctx, host, c.Script, false)
	if err != nil {
		return Result{}, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	err = cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	return result, remoteError(host, err, result.Stderr)
}

func remoteError(host string, err error, stderr string) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == 255 {
			return fmt.Errorf("%s: ssh failed: %s", host, strings.TrimSpace(stderr))
		}
		return &RemoteError{Host: host, ExitCode: exitErr.ExitCode(), Stderr: stderr}
	}
	return fmt.Errorf("%s: %w", host, err)
}

// Stream implements [Runner].
func (r *SSHRunner) Stream(ctx context.Context, host string, c Command, out io.Writer) error {
	cmd, err := r.command(ctx, host, c.Script, false)
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	// Closing stdin ends the remote watchdog's read, which stops the remote
	// process; ssh then exits on its own. WaitDelay kills it otherwise.
	cmd.Cancel = func() error { return stdin.Close() }
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = io.MultiWriter(out, &stderr)
	if err := cmd.Start(); err != nil {
		return err
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return nil //nolint:nilerr // the command was cancelled by the caller: that is not a remote failure
	}
	return remoteError(host, err, stderr.String())
}

// Interactive implements [Runner].
func (r *SSHRunner) Interactive(ctx context.Context, host, script string) error {
	cmd, err := r.command(ctx, host, script, true)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return remoteError(host, cmd.Run(), "")
}

// Close stops the master connections and removes their sockets.
func (r *SSHRunner) Close() error {
	if r.controlDir == "" {
		return nil
	}
	r.mu.Lock()
	hosts := r.hosts
	r.mu.Unlock()
	binary := r.Binary
	if binary == "" {
		binary = "ssh"
	}
	for host := range hosts {
		args := []string{"-o", "ControlPath=" + r.controlDir + "/%C"}
		if r.ConfigFile != "" {
			args = append(args, "-F", r.ConfigFile)
		}
		if r.User != "" {
			args = append(args, "-l", r.User)
		}
		if r.Port != 0 {
			args = append(args, "-p", fmt.Sprint(r.Port))
		}
		args = append(args, "-O", "exit", "--", host)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = exec.CommandContext(ctx, binary, args...).Run() // #nosec G204 -- fixed binary and validated host.
		cancel()
	}
	return os.RemoveAll(r.controlDir)
}

// LocalRunner runs local programs (git, docker build, hooks).
type LocalRunner interface {
	// Output runs argv and returns its standard output.
	Output(ctx context.Context, dir string, argv []string) (string, error)
	// Run runs argv with output streamed to the user, stdin optional and
	// extra environment appended to the process environment.
	Run(ctx context.Context, dir string, argv []string, stdin []byte, env []string) error
}

// ExecLocal is the production [LocalRunner].
type ExecLocal struct {
	Stdout, Stderr io.Writer
}

// Output implements [LocalRunner].
func (l ExecLocal) Output(ctx context.Context, dir string, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- argv built from validated config.
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// Run implements [LocalRunner].
func (l ExecLocal) Run(ctx context.Context, dir string, argv []string, stdin []byte, env []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- argv built from validated config.
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = l.Stdout, l.Stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", argv[0], err)
	}
	return nil
}
