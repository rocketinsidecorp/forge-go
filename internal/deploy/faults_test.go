package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// effectCounter numbers every external effect of an operation (a remote
// command, a stream, an interactive session, a local command) and fails the
// failAt-th one. failAt 0 fails nothing, so a healthy run counts the effects.
type effectCounter struct {
	mu     sync.Mutex
	count  int
	failAt int
	failed string
	// secret is put in the injected failure, as a leaking remote error would.
	secret string
}

func (c *effectCounter) next(host, what string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	if c.count != c.failAt {
		return nil
	}
	c.failed = what
	return &RemoteError{Host: host, ExitCode: 1, Stderr: "injected failure " + c.secret}
}

type faultRunner struct {
	inner *fakeRunner
	n     *effectCounter
	// answer overrides the result of a read-only command; ok false falls
	// through to the fake.
	answer func(host string, cmd Command) (Result, bool)
}

func (r *faultRunner) Run(ctx context.Context, host string, cmd Command) (Result, error) {
	if err := r.n.next(host, cmd.Script); err != nil {
		r.inner.mu.Lock()
		r.inner.calls = append(r.inner.calls, host+": "+cmd.Script)
		r.inner.mu.Unlock()
		return Result{}, err
	}
	if r.answer != nil {
		if result, ok := r.answer(host, cmd); ok {
			r.inner.mu.Lock()
			r.inner.calls = append(r.inner.calls, host+": "+cmd.Script)
			r.inner.mu.Unlock()
			return result, nil
		}
	}
	return r.inner.Run(ctx, host, cmd)
}

func (r *faultRunner) Stream(_ context.Context, host string, cmd Command, out io.Writer) error {
	if err := r.n.next(host, cmd.Script); err != nil {
		return err
	}
	_, _ = io.WriteString(out, "line from "+host+"\n")
	return nil
}

func (r *faultRunner) Interactive(_ context.Context, host, script string) error {
	return r.n.next(host, script)
}

func (r *faultRunner) Close() error { return nil }

type faultLocal struct{ n *effectCounter }

func (l faultLocal) Output(context.Context, string, []string) (string, error) { return "", nil }

func (l faultLocal) Run(_ context.Context, _ string, argv []string, _ []byte, _ []string) error {
	return l.n.next("local", strings.Join(argv, " "))
}

// faultOperation is one Deployer operation. surfaced reports whether the
// outcome shows the failure: an error, or a warning that says what went wrong.
type faultOperation struct {
	name string
	// tolerated lists commands whose failure the operation deliberately
	// ignores, by a substring of the script.
	tolerated []string
	run       func(ctx context.Context, d *Deployer, out io.Writer) (surfaced bool, err error)
}

func warned(report *Report, err error) bool {
	return err != nil || (report != nil && len(report.Warnings) > 0)
}

func faultOperations() []faultOperation {
	return []faultOperation{
		{name: "setup", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			report, err := d.Setup(ctx)
			return warned(report, err), err
		}},
		{name: "deploy", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			report, err := d.Deploy(ctx, DeployOptions{Version: "v3", SkipBuild: true})
			return warned(report, err), err
		}},
		{name: "deploy with a build", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			report, err := d.Deploy(ctx, DeployOptions{Version: "v3"})
			return warned(report, err), err
		}},
		{name: "rollback", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			report, err := d.Rollback(ctx, "v1")
			return warned(report, err), err
		}},
		{name: "prune", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			report, err := d.Prune(ctx)
			return warned(report, err), err
		}},
		{name: "details", tolerated: []string{"if [ -d", "tail"}, run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			details, err := d.Details(ctx)
			for _, host := range details {
				if host.Error != "" {
					return true, err
				}
			}
			return err != nil, err
		}},
		{name: "lock acquire", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			err := d.LockAcquire(ctx, "freeze")
			return err != nil, err
		}},
		{name: "lock status", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			_, err := d.LockStatus(ctx)
			return err != nil, err
		}},
		{name: "lock release", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			err := d.LockRelease(ctx, false)
			return err != nil, err
		}},
		{name: "lock release --force", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			err := d.LockRelease(ctx, true)
			return err != nil, err
		}},
		{name: "maintenance", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			err := d.Maintenance(ctx, true)
			return err != nil, err
		}},
		{name: "logs", run: func(ctx context.Context, d *Deployer, out io.Writer) (bool, error) {
			err := d.Logs(ctx, out, LogsOptions{})
			return err != nil, err
		}},
		{name: "exec", run: func(ctx context.Context, d *Deployer, out io.Writer) (bool, error) {
			err := d.Exec(ctx, out, ExecOptions{Argv: []string{"migrate", "status"}})
			return err != nil, err
		}},
		{name: "exec in the running container", run: func(ctx context.Context, d *Deployer, out io.Writer) (bool, error) {
			err := d.Exec(ctx, out, ExecOptions{Reuse: true, Argv: []string{"migrate", "status"}})
			return err != nil, err
		}},
		{name: "exec with a terminal", run: func(ctx context.Context, d *Deployer, out io.Writer) (bool, error) {
			err := d.Exec(ctx, out, ExecOptions{Interactive: true, Argv: []string{"sh"}})
			return err != nil, err
		}},
		{name: "shell", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			err := d.Shell(ctx, "", "", "")
			return err != nil, err
		}},
		{name: "shell with a debug image", run: func(ctx context.Context, d *Deployer, _ io.Writer) (bool, error) {
			err := d.Shell(ctx, "web", "192.0.2.10", "busybox:1.37")
			return err != nil, err
		}},
	}
}

// faultFixture is a planFixture whose effects are counted and can fail.
func faultFixture(t *testing.T, counter *effectCounter) (*Deployer, *fakeRunner, *bytes.Buffer) {
	t.Helper()
	d, runner, log, _ := planFixture(t, false)
	d.Config.Parallelism = 1 // one host at a time, so the n-th effect is the same one every run
	d.Runner = &faultRunner{inner: runner, n: counter, answer: func(_ string, cmd Command) (Result, bool) {
		if cmd.Script == d.cmds.lockStatus().Script {
			// A lock this performer holds, so release finds its own record.
			return Result{Stdout: `{"holder":"alice@laptop","message":"freeze","acquired_at":"2026-10-19T12:00:00Z"}` + "\n"}, true
		}
		return Result{}, false
	}}
	d.Local = faultLocal{n: counter}
	return d, runner, log
}

// TestOperationsFailClosedAtEveryStep fails each remote and local effect of
// each operation in turn. Whatever fails, the failure is visible (an error,
// or a warning for the steps that only clean up), no secret from the remote
// error reaches the caller or the log, and every lock that was taken is
// released again.
func TestOperationsFailClosedAtEveryStep(t *testing.T) {
	const secret = "postgres://shop:db-secret-2@db/shop" // a registered secret value
	for _, op := range faultOperations() {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			counter := &effectCounter{secret: secret}
			d, _, _ := faultFixture(t, counter)
			if surfaced, err := op.run(t.Context(), d, io.Discard); err != nil || surfaced {
				t.Fatalf("the operation fails on a healthy fixture: surfaced=%v err=%v", surfaced, err)
			}
			total := counter.count
			if total == 0 {
				t.Fatal("the operation did nothing")
			}
			for n := 1; n <= total; n++ {
				counter := &effectCounter{failAt: n, secret: secret}
				d, runner, log := faultFixture(t, counter)
				var out bytes.Buffer
				surfaced, err := op.run(t.Context(), d, &out)
				failed := counter.failed
				if failed == "" {
					t.Fatalf("effect %d of %d never ran", n, total)
				}
				for what, text := range map[string]string{"error": fmt.Sprint(err), "log": log.String(), "output": out.String()} {
					if strings.Contains(text, secret) {
						t.Errorf("effect %d (%.60s): the %s leaks the secret of the remote error", n, failed, what)
					}
				}
				tolerated := false
				for _, fragment := range op.tolerated {
					if strings.Contains(failed, fragment) {
						tolerated = true
					}
				}
				isRelease := strings.Contains(failed, "rm -rf .forge/shop/lock; else")
				if !surfaced && !tolerated && (!isRelease || !strings.Contains(log.String(), "warning")) {
					t.Errorf("effect %d (%.80s) failed without a trace: err=%v", n, failed, err)
				}
				// Every acquired lock is released.
				acquired, released := map[string]int{}, map[string]int{}
				for _, call := range runner.calls {
					host, script, _ := strings.Cut(call, ": ")
					switch {
					case strings.Contains(script, "if err=$(ln "):
						acquired[host]++
					case strings.Contains(script, "rm -rf .forge/shop/lock; else"):
						released[host]++
					}
				}
				if strings.Contains(failed, "if err=$(ln ") {
					// The failed acquire took no lock; the others did.
					for host, count := range acquired {
						if count > 0 && released[host] > count {
							t.Errorf("effect %d: %s released more locks than it took", n, host)
						}
					}
					continue
				}
				for host, count := range acquired {
					if op.name != "lock acquire" && released[host] != count && !isRelease {
						t.Errorf("effect %d (%.60s): %s took %d lock(s) and released %d", n, failed, host, count, released[host])
					}
				}
			}
		})
	}
}

func TestFaultHarnessCountsEffects(t *testing.T) {
	t.Parallel()
	counter := &effectCounter{failAt: 2}
	runner := &faultRunner{inner: &fakeRunner{}, n: counter}
	if _, err := runner.Run(t.Context(), "h", Command{Script: "one"}); err != nil {
		t.Fatal(err)
	}
	_, err := runner.Run(t.Context(), "h", Command{Script: "two"})
	var remote *RemoteError
	if !errors.As(err, &remote) || counter.failed != "two" {
		t.Fatalf("err=%v failed=%q", err, counter.failed)
	}
}
