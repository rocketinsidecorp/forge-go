package deploy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// queryLocal answers Output from a table and records Run calls with their
// environment.
type queryLocal struct {
	mu      sync.Mutex
	outputs map[string]string
	errs    map[string]error
	runs    [][]string
	envs    [][]string
	runErr  error
}

func (l *queryLocal) Output(_ context.Context, _ string, argv []string) (string, error) {
	key := strings.Join(argv, " ")
	if err := l.errs[key]; err != nil {
		return "", err
	}
	return l.outputs[key], nil
}

func (l *queryLocal) Run(_ context.Context, _ string, argv []string, _ []byte, env []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs = append(l.runs, argv)
	l.envs = append(l.envs, env)
	return l.runErr
}

func TestResolveVersionNeedsACleanCommitOrAnExplicitVersion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	head := "git rev-parse --short=12 --verify HEAD"
	status := "git status --porcelain --untracked-files=no"
	local := &queryLocal{outputs: map[string]string{head: "abc123def456\n", status: ""}}
	if version, err := ResolveVersion(ctx, local, ".", ""); err != nil || version != "abc123def456" {
		t.Fatalf("clean tree: %q, %v", version, err)
	}
	if version, err := ResolveVersion(ctx, local, ".", "v7"); err != nil || version != "v7" {
		t.Fatalf("explicit version: %q, %v", version, err)
	}
	if _, err := ResolveVersion(ctx, local, ".", "bad version!"); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("invalid explicit version: %v", err)
	}
	local.outputs[status] = " M main.go\n"
	if _, err := ResolveVersion(ctx, local, ".", ""); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("dirty tree: %v", err)
	}
	local.errs = map[string]error{head: errors.New("not a git repository")}
	if _, err := ResolveVersion(ctx, local, ".", ""); err == nil || !strings.Contains(err.Error(), "pass --version") {
		t.Fatalf("no git: %v", err)
	}
	local.errs = map[string]error{status: errors.New("git broke")}
	if _, err := ResolveVersion(ctx, local, ".", ""); err == nil || !strings.Contains(err.Error(), "check working tree") {
		t.Fatalf("status failure: %v", err)
	}
}

func TestEveryOperationRefusesMissingSecrets(t *testing.T) {
	t.Parallel()
	for _, op := range faultOperations() {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			d, runner, _, _ := planFixture(t, false)
			d.Secrets = NewSecrets(nil)
			_, err := op.run(t.Context(), d, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "REGISTRY_PASSWORD") {
				t.Fatalf("err = %v, want the missing secrets", err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("a command ran without its secrets: %v", runner.calls)
			}
		})
	}
}

func TestDeployAndRollbackRefuseInvalidVersions(t *testing.T) {
	t.Parallel()
	d, runner, _, _ := planFixture(t, false)
	if _, err := d.Deploy(t.Context(), DeployOptions{Version: "no spaces allowed"}); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Errorf("deploy: %v", err)
	}
	if _, err := d.Rollback(t.Context(), "../etc"); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Errorf("rollback: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("a command ran for an invalid version: %v", runner.calls)
	}
}

func TestSetupWarnsAboutAnUnpinnedProxyImage(t *testing.T) {
	t.Parallel()
	d, _, _, _ := planFixture(t, false)
	d.Config.Proxy.Image = "registry.example.com/forge-proxy:latest"
	report, err := d.Setup(t.Context())
	if err != nil || len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "not pinned by digest") {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestHooksRunInOrderWithTheirEnvironment(t *testing.T) {
	t.Parallel()
	d, _, _, _ := planFixture(t, false)
	local := &queryLocal{}
	d.Local = local
	hooks := filepath.Join(d.Root, ".forge", "hooks")
	if err := os.MkdirAll(hooks, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-build", "pre-deploy", "post-deploy"} {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte("#!/bin/sh\n"), 0o700); err != nil { // #nosec G306 -- hooks must be executable.
			t.Fatal(err)
		}
	}
	if _, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: false}); err != nil {
		t.Fatal(err)
	}
	var order []string
	for i, run := range local.runs {
		if strings.HasPrefix(run[0], hooks) {
			order = append(order, filepath.Base(run[0]))
			env := strings.Join(local.envs[i], " ")
			for _, want := range []string{"FORGE_COMMAND=deploy", "FORGE_VERSION=v3", "FORGE_SERVICE=shop", "FORGE_PERFORMER=alice@laptop"} {
				if !strings.Contains(env, want) {
					t.Errorf("hook %s lacks %s in %s", run[0], want, env)
				}
			}
		}
	}
	if strings.Join(order, ",") != "pre-build,pre-deploy,post-deploy" {
		t.Fatalf("hook order = %v", order)
	}
	if !strings.Contains(strings.Join(local.envs[len(local.envs)-1], " "), "FORGE_RUNTIME=") {
		t.Error("the post-deploy hook lacks FORGE_RUNTIME")
	}
}

func TestHookFailuresAbortWithTheHookExitCode(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*Deployer, *fakeRunner, *queryLocal, string) {
		d, runner, _, _ := planFixture(t, false)
		local := &queryLocal{}
		d.Local = local
		hooks := filepath.Join(d.Root, ".forge", "hooks")
		if err := os.MkdirAll(hooks, 0o750); err != nil {
			t.Fatal(err)
		}
		return d, runner, local, hooks
	}
	t.Run("a hook that exits non-zero", func(t *testing.T) {
		t.Parallel()
		d, runner, local, hooks := setup(t)
		if err := os.WriteFile(filepath.Join(hooks, "pre-deploy"), []byte("#!/bin/sh\n"), 0o700); err != nil { // #nosec G306 -- hooks must be executable.
			t.Fatal(err)
		}
		local.runErr = errors.New("exit status 9")
		_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
		if ExitCodeOf(err) != ExitHookFailed || !strings.Contains(err.Error(), "hook pre-deploy failed") {
			t.Fatalf("err = %v", err)
		}
		for _, call := range runner.calls {
			if strings.Contains(call, "docker start") || strings.Contains(call, "docker run") {
				t.Fatalf("a container started after a failed hook: %s", call)
			}
		}
	})
	t.Run("a hook that is not executable", func(t *testing.T) {
		t.Parallel()
		d, _, _, hooks := setup(t)
		if err := os.WriteFile(filepath.Join(hooks, "pre-deploy"), []byte("#!/bin/sh\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
		if ExitCodeOf(err) != ExitHookFailed || !strings.Contains(err.Error(), "not executable") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a hook that is a directory", func(t *testing.T) {
		t.Parallel()
		d, _, _, hooks := setup(t)
		if err := os.Mkdir(filepath.Join(hooks, "pre-deploy"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
		if ExitCodeOf(err) != ExitHookFailed {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a hook that cannot be examined", func(t *testing.T) {
		t.Parallel()
		d, _, _, hooks := setup(t)
		// A path component that is a file makes Stat fail with ENOTDIR.
		if err := os.WriteFile(filepath.Join(hooks, "pre-deploy"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		d.Config.Hooks = filepath.Join(".forge", "hooks", "pre-deploy")
		_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
		if ExitCodeOf(err) != ExitHookFailed || !strings.Contains(err.Error(), "hook pre-deploy") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a failing build step is an error before any remote command", func(t *testing.T) {
		t.Parallel()
		d, runner, local, _ := setup(t)
		local.runErr = errors.New("docker build exploded")
		_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3"})
		if err == nil || !strings.Contains(err.Error(), "registry login") {
			t.Fatalf("err = %v", err)
		}
		for _, call := range runner.calls {
			if strings.Contains(call, "docker pull") {
				t.Fatalf("pulled after a failed build: %s", call)
			}
		}
	})
}

func TestSwitchRevertFailureIsReportedAsInconsistent(t *testing.T) {
	t.Parallel()
	d, runner, log, _ := planFixture(t, false)
	d.Config.Parallelism = 1
	// Host .11 fails its health check, so .10 (switched first) must be
	// reverted; the revert itself fails, which must be called out.
	runner.fail = func(host string, cmd Command) error {
		switch {
		case host == "192.0.2.11" && strings.Contains(cmd.Script, "forge-proxy deploy"):
			return &RemoteError{Host: host, ExitCode: 4, Stderr: "unhealthy"}
		case host == "192.0.2.10" && (strings.Contains(cmd.Script, "docker stop") || strings.HasSuffix(strings.TrimSpace(cmd.Script), "shop-web-v2-bbbbbb") && strings.Contains(cmd.Script, "forge-proxy deploy")):
			return &RemoteError{Host: host, ExitCode: 1, Stderr: "cannot revert"}
		}
		return nil
	}
	_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
	if ExitCodeOf(err) != ExitInconsistent || !strings.Contains(err.Error(), "reverting also failed") {
		t.Fatalf("err = %v (exit %d)\n%s", err, ExitCodeOf(err), log.String())
	}
}

func TestSwitchInterruptedIsReportedAsInconsistent(t *testing.T) {
	t.Parallel()
	d, runner, _, _ := planFixture(t, false)
	d.Config.Parallelism = 1
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner.fail = func(host string, cmd Command) error {
		if strings.Contains(cmd.Script, "docker run --detach") {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	_, err := d.Deploy(ctx, DeployOptions{Version: "v3", SkipBuild: true})
	if ExitCodeOf(err) != ExitInconsistent || !strings.Contains(err.Error(), "interrupted while switching") {
		t.Fatalf("err = %v (exit %d)", err, ExitCodeOf(err))
	}
}

func TestRollbackSkipsRolesThatAlreadyRunTheVersionAndNamesWhatIsMissing(t *testing.T) {
	t.Parallel()
	d, runner, log, _ := planFixture(t, false)
	report, err := d.Rollback(t.Context(), "v2")
	if err != nil || len(report.Steps) != 0 || !strings.Contains(log.String(), "already runs v2") {
		t.Fatalf("report=%+v err=%v\n%s", report, err, log.String())
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "docker start") {
			t.Fatalf("restarted a container that already serves: %s", call)
		}
	}
	if _, err := d.Rollback(t.Context(), "v404"); err == nil || !strings.Contains(err.Error(), "not retained") || !strings.Contains(err.Error(), "available: v2, v1, v0") {
		t.Fatalf("missing version: %v", err)
	}
	// A host with no container of the role reports "none" as available.
	if got := versionsOf(nil, "web"); len(got) != 1 || got[0] != "none" {
		t.Fatalf("versionsOf(nil) = %v", got)
	}
}

func TestLockReleaseRefusesWhatIsNotOurs(t *testing.T) {
	t.Parallel()
	mine := `{"holder":"alice@laptop","message":"freeze","acquired_at":"2026-10-19T12:00:00Z"}` + "\n"
	answer := func(stdout string, releaseExit int) func(*Deployer, *fakeRunner) {
		return func(d *Deployer, runner *fakeRunner) {
			runner.fail = func(host string, cmd Command) error {
				if releaseExit != 0 && strings.Contains(cmd.Script, "rm -rf .forge/shop/lock; else") {
					return &RemoteError{Host: host, ExitCode: releaseExit}
				}
				return nil
			}
			d.Runner = &faultRunner{inner: runner, n: &effectCounter{}, answer: func(_ string, cmd Command) (Result, bool) {
				if cmd.Script == d.cmds.lockStatus().Script {
					return Result{Stdout: stdout}, true
				}
				return Result{}, false
			}}
		}
	}
	for name, tc := range map[string]struct {
		setup   func(*Deployer, *fakeRunner)
		wantErr string
		code    int
	}{
		"held by someone else": {answer(`{"holder":"bob@ci","message":"window","acquired_at":"2026-10-19T12:00:00Z"}`+"\n", 0), "held by bob@ci", ExitLockHeld},
		"without a record":     {answer("{}\n", 0), "pass --force", ExitLockHeld},
		"gone meanwhile":       {answer(mine, lockGoneExitStatus), "", 0},
		"taken meanwhile":      {answer(mine, lockNotOursExitStatus), "the lock changed while releasing", ExitLockHeld},
		"released":             {answer(mine, 0), "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, runner, log, _ := planFixture(t, false)
			tc.setup(d, runner)
			err := d.LockRelease(t.Context(), false)
			if ExitCodeOf(err) != tc.code || (tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr))) {
				t.Fatalf("err = %v (exit %d)", err, ExitCodeOf(err))
			}
			if name == "gone meanwhile" && !strings.Contains(log.String(), "already released") {
				t.Errorf("log lacks the already-released notice:\n%s", log.String())
			}
		})
	}
}

func TestLockRecordsAreParsedDefensively(t *testing.T) {
	t.Parallel()
	for _, stdout := range []string{"", "{}", "  {}\n"} {
		if info := parseLockInfo(stdout); info.Holder != "unknown" || !strings.Contains(info.Message, "without a record") {
			t.Errorf("parseLockInfo(%q) = %+v", stdout, info)
		}
	}
	if info := parseLockInfo("{not json"); info.Holder != "unknown" || info.Message != "unreadable lock record" {
		t.Errorf("garbage record: %+v", info)
	}
	info := parseLockInfo(`{"holder":"alice@laptop","message":"m","acquired_at":"2026-10-19T12:00:00Z"}`)
	if info.Holder != "alice@laptop" || info.AcquiredAt.Year() != 2026 {
		t.Errorf("record: %+v", info)
	}
}

func TestInventoryOutputThatIsNotJSONIsAnError(t *testing.T) {
	t.Parallel()
	d, runner, _, _ := planFixture(t, false)
	runner.inventory["192.0.2.10"] = "this is not json\n"
	if _, err := d.Prune(t.Context()); err == nil || !strings.Contains(err.Error(), "unexpected docker ps output") {
		t.Fatalf("err = %v", err)
	}
	if containers, err := parseInventory("\n\n"); err != nil || len(containers) != 0 {
		t.Fatalf("blank lines: %v %v", containers, err)
	}
}

func TestMaintenanceNeedsAProxiedRole(t *testing.T) {
	t.Parallel()
	d, _, _, _ := planFixture(t, false)
	for name, role := range d.Config.Servers {
		off := false
		role.Proxy = &off
		d.Config.Servers[name] = role
	}
	if err := d.Maintenance(t.Context(), true); err == nil || !strings.Contains(err.Error(), "no role uses the proxy") {
		t.Fatalf("err = %v", err)
	}
}

func TestDetailsReportsProxyAndLockAndHostFailures(t *testing.T) {
	t.Parallel()
	d, runner, _, _ := planFixture(t, false)
	d.Config.Parallelism = 1
	runner.fail = func(host string, cmd Command) error {
		if host == "192.0.2.11" && strings.Contains(cmd.Script, "forge-proxy status") {
			return &RemoteError{Host: host, ExitCode: 1, Stderr: "proxy down"}
		}
		return nil
	}
	d.Runner = &faultRunner{inner: runner, n: &effectCounter{}, answer: func(host string, cmd Command) (Result, bool) {
		switch {
		case strings.Contains(cmd.Script, "forge-proxy status") && host == "192.0.2.10":
			return Result{Stdout: `{"services":[{"service":"shop","target":"shop-web-v2-bbbbbb:8080"}]}`}, true
		case cmd.Script == d.cmds.lockStatus().Script:
			return Result{Stdout: `{"holder":"bob@ci","message":"window","acquired_at":"2026-10-19T12:00:00Z"}`}, true
		case strings.HasPrefix(cmd.Script, "tail"):
			return Result{Stdout: "{\"version\":\"v2\"}\nnot json\n{\"version\":\"v1\"}\n"}, true
		}
		return Result{}, false
	}}
	details, err := d.Details(t.Context())
	if err != nil || len(details) != 2 {
		t.Fatalf("details=%+v err=%v", details, err)
	}
	first, second := details[0], details[1]
	if len(first.Proxy) != 1 || first.Lock == nil || first.Lock.Holder != "bob@ci" || len(first.Deploys) != 2 || first.Error != "" {
		t.Fatalf("first host = %+v", first)
	}
	if !strings.Contains(second.Error, "forge-proxy:") || len(second.Proxy) != 0 {
		t.Fatalf("second host = %+v", second)
	}
}

func TestLogsRefuseInvalidOptions(t *testing.T) {
	t.Parallel()
	d, runner, _, _ := planFixture(t, false)
	for name, opts := range map[string]LogsOptions{
		"unknown role": {Role: "worker"},
		"unknown host": {Host: "192.0.2.99"},
		"bad since":    {Since: "1h; reboot"},
		"negative":     {Lines: -1},
		"too many":     {Lines: 100001},
		"bad grep":     {Grep: "("},
	} {
		if err := d.Logs(t.Context(), &bytes.Buffer{}, opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(runner.calls) != 0 {
		t.Errorf("commands ran for invalid options: %v", runner.calls)
	}
}

func TestExecAndShellRefuseInvalidInput(t *testing.T) {
	t.Parallel()
	d, runner, _, _ := planFixture(t, false)
	ctx := t.Context()
	if err := d.Exec(ctx, &bytes.Buffer{}, ExecOptions{}); err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Errorf("no command: %v", err)
	}
	if err := d.Exec(ctx, &bytes.Buffer{}, ExecOptions{Argv: []string{"ls", "bad\narg"}}); err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Errorf("control characters: %v", err)
	}
	if err := d.Exec(ctx, &bytes.Buffer{}, ExecOptions{Role: "worker", Argv: []string{"ls"}}); err == nil {
		t.Error("unknown role")
	}
	if err := d.Shell(ctx, "worker", "", ""); err == nil {
		t.Error("shell: unknown role")
	}
	if err := d.Shell(ctx, "", "", "not an image;"); err == nil || !strings.Contains(err.Error(), "not an image reference") {
		t.Errorf("shell: bad image: %v", err)
	}
	if err := d.Shell(ctx, "", "192.0.2.99", ""); err == nil || !strings.Contains(err.Error(), "does not run role") {
		t.Errorf("shell: foreign host: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("commands ran for invalid input: %v", runner.calls)
	}
}

func TestPrefixWriterFlushesLongAndUnterminatedLines(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	writer := &prefixWriter{prefix: "h|", out: &lockedOutput{w: &out}, redactor: NewSecrets(nil).Redactor()}
	// An unterminated tail is emitted on flush, a CR is dropped, and a line
	// longer than the buffer cap is cut instead of growing without bound.
	if _, err := writer.Write([]byte("first\r\nsecond")); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 70<<10)
	if _, err := writer.Write([]byte(long)); err != nil {
		t.Fatal(err)
	}
	writer.flush()
	writer.flush() // nothing left: no extra line
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "h|first" || !strings.HasPrefix(lines[1], "h|second"+strings.Repeat("x", 100)) {
		t.Fatalf("lines = %d, first=%.20q", len(lines), lines)
	}
	if len(lines[1]) != len("h|second")+len(long) {
		t.Fatalf("the long line lost bytes: %d", len(lines[1]))
	}
}

func TestRemoteErrorTextIsBoundedAndNeverEmpty(t *testing.T) {
	t.Parallel()
	if got := (&RemoteError{Host: "h", ExitCode: 7}).Error(); got != "h: remote command exited with status 7" {
		t.Errorf("empty stderr: %q", got)
	}
	long := (&RemoteError{Host: "h", ExitCode: 1, Stderr: strings.Repeat("e", 5000)}).Error()
	if !strings.HasSuffix(long, "...") || len(long) > 2100 {
		t.Errorf("long stderr: %d bytes", len(long))
	}
}

func TestDeployerWithoutOptionalFieldsUsesDefaults(t *testing.T) {
	t.Parallel()
	d, _, _, _ := planFixture(t, false)
	d.Log, d.Now, d.Suffix, d.Performer, d.Root = nil, nil, nil, "", ""
	if err := d.prepare(); err != nil {
		t.Fatal(err)
	}
	if d.Log == nil || d.Now == nil || d.Suffix == nil || d.Performer == "" || d.Root != "." {
		t.Fatalf("defaults not applied: %+v", d)
	}
	if got := d.Suffix(); len(got) != 6 {
		t.Errorf("suffix %q", got)
	}
	if time.Since(d.Now()) > time.Minute {
		t.Error("the default clock is not the wall clock")
	}
	if !strings.Contains(Performer(), "@") {
		t.Errorf("Performer() = %q", Performer())
	}
}
