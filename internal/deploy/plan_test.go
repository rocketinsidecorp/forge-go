package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingLocal records local commands instead of running them.
type recordingLocal struct {
	mu    sync.Mutex
	calls [][]string
}

func (l *recordingLocal) Output(_ context.Context, _ string, argv []string) (string, error) {
	return "", fmt.Errorf("unexpected local query %v", argv)
}

func (l *recordingLocal) Run(_ context.Context, _ string, argv []string, _ []byte, _ []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, argv)
	return nil
}

var update = flag.Bool("update", false, "rewrite golden files")

// fakeRunner answers read-only queries from fixtures and records every
// command. fail, when set, decides the error of a command.
type fakeRunner struct {
	mu        sync.Mutex
	calls     []string
	inventory map[string]string
	images    map[string]string
	fail      func(host string, cmd Command) error
}

func (f *fakeRunner) Run(_ context.Context, host string, cmd Command) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, host+": "+cmd.Script)
	if f.fail != nil {
		if err := f.fail(host, cmd); err != nil {
			return Result{}, err
		}
	}
	switch {
	case strings.HasPrefix(cmd.Script, "docker ps --all"):
		return Result{Stdout: f.inventory[host]}, nil
	case strings.HasPrefix(cmd.Script, "docker image ls"):
		return Result{Stdout: f.images[host]}, nil
	}
	return Result{}, nil
}

func (f *fakeRunner) Stream(context.Context, string, Command, io.Writer) error { return nil }
func (f *fakeRunner) Interactive(context.Context, string, string) error        { return nil }
func (f *fakeRunner) Close() error                                             { return nil }

func psJSON(name, role, version, state string, created time.Time) string {
	line, _ := json.Marshal(map[string]string{
		"Names": name, "State": state, "Image": "registry.example.com/acme/shop:" + version,
		"Labels":    fmt.Sprintf("forge.role=%s,forge.service=shop,forge.version=%s", role, version),
		"CreatedAt": created.UTC().Format("2006-01-02 15:04:05 -0700 MST"),
	})
	return string(line) + "\n"
}

func imageJSON(tag string) string {
	return fmt.Sprintf(`{"Repository":"registry.example.com/acme/shop","Tag":%q}`+"\n", tag)
}

var planEpoch = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// planFixture: web on .10 and .11, jobs on .11; v2 is live, v1 and v0 are
// retained, retain is 2.
func planFixture(t *testing.T, dryRun bool) (*Deployer, *fakeRunner, *bytes.Buffer, *recordingLocal) {
	t.Helper()
	cfg, err := ParseConfig([]byte(strings.Replace(validConfig, "retain: 3", "retain: 2", 1)))
	if err != nil {
		t.Fatal(err)
	}
	inventory := func(role string) string {
		return psJSON("shop-"+role+"-v2-bbbbbb", role, "v2", "running", planEpoch.Add(-time.Hour)) +
			psJSON("shop-"+role+"-v1-aaaaaa", role, "v1", "exited", planEpoch.Add(-2*time.Hour)) +
			psJSON("shop-"+role+"-v0-999999", role, "v0", "exited", planEpoch.Add(-3*time.Hour))
	}
	runner := &fakeRunner{
		inventory: map[string]string{
			"192.0.2.10": inventory("web"),
			"192.0.2.11": inventory("web") + inventory("jobs"),
		},
		images: map[string]string{
			"192.0.2.10": imageJSON("v3") + imageJSON("v2") + imageJSON("v1") + imageJSON("v0"),
			"192.0.2.11": imageJSON("v2") + imageJSON("v1") + imageJSON("v0") + imageJSON("<none>"),
		},
	}
	counter := 0
	log := &bytes.Buffer{}
	local := &recordingLocal{}
	d := &Deployer{
		Config: cfg,
		Secrets: NewSecrets(map[string]string{
			"REGISTRY_PASSWORD": "registry-secret-1", "FORGE_DATABASE_URL": "postgres://shop:db-secret-2@db/shop", "POSTGRES_PASSWORD": "pg-secret-3",
		}),
		Runner: runner, Local: local, Root: t.TempDir(), Log: log, Performer: "alice@laptop",
		Now:    func() time.Time { return planEpoch },
		Suffix: func() string { counter++; return fmt.Sprintf("%06d", counter) },
		DryRun: dryRun,
	}
	return d, runner, log, local
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) // #nosec G304 -- test fixture.
	if err != nil {
		t.Fatalf("%v (run go test ./internal/deploy -run Golden -update)", err)
	}
	if got != string(want) {
		t.Fatalf("%s plan differs from %s (rerun with -update after review):\n%s", name, path, got)
	}
	for _, secret := range []string{"registry-secret-1", "db-secret-2", "pg-secret-3"} {
		if strings.Contains(got, secret) {
			t.Fatalf("%s plan leaks a secret", name)
		}
	}
}

func TestGoldenDeployPlan(t *testing.T) {
	d, runner, log, _ := planFixture(t, true)
	if _, err := d.Deploy(t.Context(), DeployOptions{Version: "v3"}); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "deploy", log.String())
	for _, call := range runner.calls {
		if !strings.HasPrefix(strings.SplitN(call, ": ", 2)[1], "docker ps --all") && !strings.HasPrefix(strings.SplitN(call, ": ", 2)[1], "docker image ls") {
			t.Fatalf("dry run executed a mutating command: %s", call)
		}
	}
}

func TestGoldenRollbackPlan(t *testing.T) {
	d, _, log, _ := planFixture(t, true)
	if _, err := d.Rollback(t.Context(), "v1"); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "rollback", log.String())
}

func TestGoldenPrunePlan(t *testing.T) {
	d, _, log, _ := planFixture(t, true)
	if _, err := d.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "prune", log.String())
}

func TestGoldenSetupPlan(t *testing.T) {
	d, _, log, _ := planFixture(t, true)
	report, err := d.Setup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("digest-pinned proxy image warned: %v", report.Warnings)
	}
	checkGolden(t, "setup", log.String())
}

func TestDeployReportAndBuild(t *testing.T) {
	d, runner, _, local := planFixture(t, false)
	report, err := d.Deploy(t.Context(), DeployOptions{Version: "v3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(local.calls) != 3 || local.calls[0][1] != "login" || local.calls[1][1] != "build" || local.calls[2][1] != "push" {
		t.Fatalf("local calls = %v", local.calls)
	}
	if got := strings.Join(local.calls[1], " "); !strings.Contains(got, "--platform linux/amd64 --build-arg GO_VERSION=1.27 .") {
		t.Fatalf("build argv = %s", got)
	}
	if len(report.Steps) != 3 || report.Steps[0].Container != "shop-web-v3-000002" || report.Steps[0].Stopped[0] != "shop-web-v2-bbbbbb" {
		t.Fatalf("steps = %+v", report.Steps)
	}
	// retain 2: v3 and v2 stay, v1 and v0 go, with their images.
	want := []string{"192.0.2.10:registry.example.com/acme/shop:v0", "192.0.2.10:registry.example.com/acme/shop:v1", "192.0.2.10:shop-web-v1-aaaaaa", "192.0.2.10:shop-web-v0-999999"}
	for _, w := range want {
		found := false
		for _, p := range report.Pruned {
			found = found || p == w
		}
		if !found {
			t.Fatalf("pruned %v lacks %s", report.Pruned, w)
		}
	}
	if strings.Contains(strings.Join(runner.calls, "\n"), "image rm registry.example.com/acme/shop:v3") {
		t.Fatal("prune removed the image just deployed")
	}
	if !strings.Contains(strings.Join(runner.calls, "\n"), "cat >> .forge/shop/deploys.log") {
		t.Fatal("deploy not recorded")
	}
}

func TestDeployAbortsOnFailingHookBeforeRemoteChanges(t *testing.T) {
	d, runner, _, _ := planFixture(t, false)
	d.Local = ExecLocal{Stdout: io.Discard, Stderr: io.Discard}
	hooks := filepath.Join(d.Root, ".forge", "hooks")
	if err := os.MkdirAll(hooks, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(d.Root, "hook-env")
	script := "#!/bin/sh\nenv | grep '^FORGE_' | sort > " + marker + "\nexit 3\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-deploy"), []byte(script), 0o700); err != nil { // #nosec G306 -- hook must be executable.
		t.Fatal(err)
	}
	_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
	if ExitCodeOf(err) != ExitHookFailed {
		t.Fatalf("err = %v (exit %d)", err, ExitCodeOf(err))
	}
	envDump, _ := os.ReadFile(marker)
	for _, want := range []string{"FORGE_COMMAND=deploy", "FORGE_VERSION=v3", "FORGE_SERVICE=shop", "FORGE_HOSTS=192.0.2.10,192.0.2.11", "FORGE_ROLES=web,jobs", "FORGE_PERFORMER=alice@laptop"} {
		if !strings.Contains(string(envDump), want) {
			t.Fatalf("hook env lacks %s:\n%s", want, envDump)
		}
	}
	for _, call := range runner.calls {
		script := strings.SplitN(call, ": ", 2)[1]
		if !strings.Contains(script, ".forge/shop/lock") {
			t.Fatalf("remote command ran after the hook failed: %s", call)
		}
	}
	if err := os.Chmod(filepath.Join(hooks, "pre-deploy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true}); ExitCodeOf(err) != ExitHookFailed || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("non-executable hook: %v", err)
	}
}

func TestProxySwitchFailureWithoutPreviousKeepsHealthyHosts(t *testing.T) {
	d, runner, _, _ := planFixture(t, false)
	runner.inventory = map[string]string{}
	runner.fail = func(host string, cmd Command) error {
		if host == "192.0.2.11" && strings.Contains(cmd.Script, "forge-proxy deploy") {
			return &RemoteError{Host: host, ExitCode: 4, Stderr: "not healthy"}
		}
		return nil
	}
	_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
	if ExitCodeOf(err) != ExitUnhealthy {
		t.Fatalf("err = %v", err)
	}
	calls := strings.Join(runner.calls, "\n")
	if strings.Contains(calls, "192.0.2.10: docker stop -t 35 shop-web-v3") {
		t.Fatalf("first deploy removed the only healthy container:\n%s", calls)
	}
	if !strings.Contains(calls, "192.0.2.11: docker rm shop-web-v3") {
		t.Fatalf("failed container not removed:\n%s", calls)
	}
}

func TestRevertFailureIsInconsistent(t *testing.T) {
	d, runner, _, _ := planFixture(t, false)
	runner.fail = func(host string, cmd Command) error {
		if host == "192.0.2.11" && strings.Contains(cmd.Script, "forge-proxy deploy") {
			return &RemoteError{Host: host, ExitCode: 4, Stderr: "not healthy"}
		}
		if host == "192.0.2.10" && strings.Contains(cmd.Script, "-target shop-web-v2-bbbbbb") {
			return &RemoteError{Host: host, ExitCode: 1, Stderr: "proxy gone"}
		}
		return nil
	}
	_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
	if ExitCodeOf(err) != ExitInconsistent || !strings.Contains(err.Error(), "proxy gone") {
		t.Fatalf("err = %v (exit %d)", err, ExitCodeOf(err))
	}
}

func TestLockHeldIsReported(t *testing.T) {
	d, runner, _, _ := planFixture(t, false)
	runner.fail = func(host string, cmd Command) error {
		if host == "192.0.2.11" && strings.Contains(cmd.Script, "mkdir .forge/shop/lock") {
			return &RemoteError{Host: host, ExitCode: lockHeldExitStatus}
		}
		return nil
	}
	_, err := d.Deploy(t.Context(), DeployOptions{Version: "v3", SkipBuild: true})
	if ExitCodeOf(err) != ExitLockHeld {
		t.Fatalf("err = %v", err)
	}
	calls := strings.Join(runner.calls, "\n")
	if !strings.Contains(calls, "192.0.2.10: rm -rf .forge/shop/lock") || strings.Contains(calls, "192.0.2.11: rm -rf") {
		t.Fatalf("partial lock not released correctly:\n%s", calls)
	}
}

func TestParseAllAcceptsFlagsAnywhere(t *testing.T) {
	flags, common := newFlags("t", io.Discard, true)
	positional, err := parseAll(flags, []string{"v1", "--json", "--dry-run", "--", "--literal"})
	if err != nil || !common.json || !common.dryRun || strings.Join(positional, " ") != "v1 --literal" {
		t.Fatalf("positional=%v json=%v dry=%v err=%v", positional, common.json, common.dryRun, err)
	}
}

func TestRunDeployReportsConfigErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := RunDeploy([]string{"--config", filepath.Join(t.TempDir(), "missing.yml")}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "read deploy config") {
		t.Fatalf("err = %v", err)
	}
	if err := RunDeploy([]string{"explode"}, &stdout, &stderr); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
	if err := RunApp([]string{"dance"}, &stdout, &stderr); err == nil {
		t.Fatal("unknown app subcommand accepted")
	}
	if err := RunLogs([]string{"extra"}, &stdout, &stderr); err == nil {
		t.Fatal("logs accepted a positional argument")
	}
}

func TestRunDeployInit(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("go.mod", []byte("module example.com/storefront\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := RunDeploy([]string{"init"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(DefaultConfigPath)
	if err != nil || cfg.Service != "storefront" {
		t.Fatalf("init config: %v %+v", err, cfg)
	}
	if !strings.Contains(stdout.String(), "wrote .forge/secrets") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

// Retention must treat the container just started as the newest even when
// the host clock that stamped the others is ahead of the local clock.
func TestApplyBootIsNewestDespiteClockSkew(t *testing.T) {
	hostAhead := planEpoch.Add(time.Hour)
	containers := []Container{
		{Name: "shop-web-v2-b", Role: "web", Version: "v2", State: "running", Created: hostAhead},
		{Name: "shop-web-v1-a", Role: "web", Version: "v1", State: "exited", Created: hostAhead.Add(-time.Minute)},
	}
	b := &boot{role: "web", version: "v3", name: "shop-web-v3-c", previous: containers[:1]}
	got := applyBoot(containers, b, planEpoch)
	if got[0].Name != "shop-web-v3-c" || !got[0].Created.After(hostAhead) {
		t.Fatalf("new container not newest: %+v", got[0])
	}
	d, runner, _, _ := planFixture(t, false)
	d.Config.Retain = 2
	if err := d.prepare(); err != nil {
		t.Fatal(err)
	}
	pruned, _ := d.pruneHost(t.Context(), "192.0.2.10", got)
	if len(pruned) == 0 || pruned[0] != "192.0.2.10:shop-web-v1-a" {
		t.Fatalf("pruned %v, want the oldest version v1; calls %v", pruned, runner.calls)
	}
}
