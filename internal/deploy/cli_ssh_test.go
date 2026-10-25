//go:build unix

package deploy

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The command line (`forge deploy ...`, `forge logs`, `forge app ...`) runs
// against the private sshd cluster of the integration tests. The commands
// build their own SSH runner, which reads the user's ssh configuration; a
// wrapper first on PATH runs the real ssh with the cluster's configuration
// instead, so the tests never read the real ~/.ssh.

// cliCluster is a cluster plus the files and wrapper the commands need.
type cliCluster struct {
	*sshCluster
	config, secrets string
}

func newCLICluster(t *testing.T) *cliCluster {
	t.Helper()
	cluster := startCluster(t, "alpha", "beta")
	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Skipf("ssh not installed: %v", err)
	}
	bin := filepath.Join(cluster.dir, "clibin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nexec " + realSSH + " -F " + cluster.sshConfig + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(wrapper), 0o700); err != nil { // #nosec G306 -- test wrapper must be executable.
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := &cliCluster{sshCluster: cluster, config: filepath.Join(cluster.dir, "deploy.yml"), secrets: filepath.Join(cluster.dir, "secrets")}
	if err := os.WriteFile(c.config, []byte(integrationConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := "REGISTRY_PASSWORD=" + itRegistryPassword + "\nFORGE_DATABASE_URL=" + itDatabaseURL + "\nPOSTGRES_PASSWORD=" + itPostgresPassword + "\n"
	if err := os.WriteFile(c.secrets, []byte(secrets), 0o600); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *cliCluster) deploy(args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	err = RunDeploy(append(append([]string{}, args...), "--config", c.config, "--secrets", c.secrets), &out, &errOut)
	return out.String(), errOut.String(), err
}

func (c *cliCluster) logs(args ...string) (string, error) {
	var out, errOut bytes.Buffer
	err := RunLogs(append(append([]string{}, args...), "--config", c.config, "--secrets", c.secrets), &out, &errOut)
	return out.String(), err
}

func (c *cliCluster) app(args ...string) (string, error) {
	var out, errOut bytes.Buffer
	args = append([]string{args[0], "--config", c.config, "--secrets", c.secrets}, args[1:]...)
	err := RunApp(args, &out, &errOut)
	return out.String(), err
}

func TestCommandLineAgainstSSH(t *testing.T) {
	// Not parallel: PATH is replaced for the commands' own ssh.
	c := newCLICluster(t)

	mustSucceed := func(t *testing.T, args ...string) string {
		t.Helper()
		stdout, stderr, err := c.deploy(args...)
		if err != nil {
			t.Fatalf("forge deploy %v: %v\nstdout: %s\nstderr: %s", args, err, stdout, stderr)
		}
		return stdout
	}

	t.Run("setup prints the summary", func(t *testing.T) {
		out := mustSucceed(t, "setup")
		if !strings.Contains(out, "setup complete (") {
			t.Fatalf("stdout = %q", out)
		}
		// A JSON report is schema-versioned and says it succeeded.
		var report struct {
			OK       bool `json:"ok"`
			ExitCode int  `json:"exit_code"`
			DryRun   bool `json:"dry_run"`
		}
		if err := json.Unmarshal([]byte(mustSucceed(t, "setup", "--json")), &report); err != nil || !report.OK || report.ExitCode != 0 || report.DryRun {
			t.Fatalf("report = %+v err=%v", report, err)
		}
	})

	t.Run("deploy and its JSON report", func(t *testing.T) {
		out := mustSucceed(t, "--version", "v1", "--skip-build")
		if !strings.Contains(out, "deployed v1 (") {
			t.Fatalf("stdout = %q", out)
		}
		var report struct {
			OK    bool `json:"ok"`
			Steps []struct {
				Role string `json:"role"`
			} `json:"steps"`
		}
		if err := json.Unmarshal([]byte(mustSucceed(t, "--version", "v2", "--skip-build", "--json")), &report); err != nil || !report.OK || len(report.Steps) == 0 {
			t.Fatalf("report = %+v err=%v", report, err)
		}
		if v := routeVersion(t, c.state(t, "alpha")); v != "v2" {
			t.Fatalf("alpha routes to %s", v)
		}
	})

	t.Run("a dry run prints the plan and changes nothing", func(t *testing.T) {
		before := c.state(t, "alpha")
		out := mustSucceed(t, "--version", "v3", "--skip-build", "--dry-run")
		if !strings.Contains(out, "dry run: nothing was changed") || !strings.Contains(out, "forge-proxy deploy -service shop") {
			t.Fatalf("stdout = %q", out)
		}
		if strings.Contains(out, itDatabaseURL) || strings.Contains(out, itRegistryPassword) {
			t.Fatal("the plan leaks a secret")
		}
		if after := c.state(t, "alpha"); len(after.Containers) != len(before.Containers) {
			t.Fatal("a dry run changed the cluster")
		}
		// A dry-run JSON report keeps the plan on stderr and says so.
		stdout, _, err := c.deploy("--version", "v3", "--skip-build", "--dry-run", "--json")
		var report struct {
			DryRun bool `json:"dry_run"`
		}
		if err != nil || json.Unmarshal([]byte(stdout), &report) != nil || !report.DryRun {
			t.Fatalf("stdout = %q err=%v", stdout, err)
		}
	})

	t.Run("details in text and JSON", func(t *testing.T) {
		out := mustSucceed(t, "details")
		for _, want := range []string{"alpha (web)", "beta (web, jobs)", "proxy    shop -> ", "history  "} {
			if !strings.Contains(out, want) {
				t.Errorf("details lacks %q:\n%s", want, out)
			}
		}
		var document struct {
			SchemaVersion int    `json:"schema_version"`
			Service       string `json:"service"`
			Hosts         []struct {
				Host string `json:"host"`
			} `json:"hosts"`
		}
		if err := json.Unmarshal([]byte(mustSucceed(t, "details", "--json")), &document); err != nil || document.SchemaVersion != 1 || document.Service != "shop" || len(document.Hosts) != 2 {
			t.Fatalf("document = %+v err=%v", document, err)
		}
	})

	t.Run("locks", func(t *testing.T) {
		if out := mustSucceed(t, "lock", "status"); !strings.Contains(out, "not locked") {
			t.Fatalf("status = %q", out)
		}
		if out := mustSucceed(t, "lock", "acquire", "--message", "freeze"); !strings.Contains(out, "lock acquired") {
			t.Fatalf("acquire = %q", out)
		}
		out := mustSucceed(t, "lock", "status")
		if !strings.Contains(out, "alpha: held by ") || !strings.Contains(out, "(freeze)") {
			t.Fatalf("status = %q", out)
		}
		var document struct {
			Locked bool `json:"locked"`
		}
		if err := json.Unmarshal([]byte(mustSucceed(t, "lock", "status", "--json")), &document); err != nil || !document.Locked {
			t.Fatalf("document = %+v err=%v", document, err)
		}
		// A deploy while the lock is held fails with the lock-held exit code,
		// and its JSON report says so.
		stdout, _, err := c.deploy("--version", "v9", "--skip-build", "--json")
		if ExitCodeOf(err) != ExitLockHeld {
			t.Fatalf("deploy while locked: %v", err)
		}
		var report struct {
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
			ExitCode int    `json:"exit_code"`
		}
		if json.Unmarshal([]byte(stdout), &report) != nil || report.OK || report.ExitCode != ExitLockHeld || report.Error == "" {
			t.Fatalf("report = %+v from %q", report, stdout)
		}
		if out := mustSucceed(t, "lock", "release"); !strings.Contains(out, "lock released") {
			t.Fatalf("release = %q", out)
		}
		if out := mustSucceed(t, "lock", "status"); !strings.Contains(out, "not locked") {
			t.Fatalf("status after release = %q", out)
		}
	})

	t.Run("maintenance", func(t *testing.T) {
		mustSucceed(t, "maintenance", "on")
		if !c.state(t, "alpha").Maintenance["shop"] {
			t.Fatal("maintenance mode was not switched on")
		}
		mustSucceed(t, "maintenance", "off")
		if c.state(t, "alpha").Maintenance["shop"] {
			t.Fatal("maintenance mode was not switched off")
		}
	})

	t.Run("rollback and prune", func(t *testing.T) {
		if out := mustSucceed(t, "rollback", "v1"); !strings.Contains(out, "rolled back to v1 (") {
			t.Fatalf("rollback = %q", out)
		}
		if v := routeVersion(t, c.state(t, "alpha")); v != "v1" {
			t.Fatalf("alpha routes to %s after the rollback", v)
		}
		var report struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal([]byte(mustSucceed(t, "prune", "--json")), &report); err != nil || !report.OK {
			t.Fatalf("prune report = %+v err=%v", report, err)
		}
		if out := mustSucceed(t, "prune"); !strings.Contains(out, "prune complete (") {
			t.Fatalf("prune = %q", out)
		}
		// A failing rollback is an error in JSON too.
		stdout, _, err := c.deploy("rollback", "v404", "--json")
		if err == nil || !strings.Contains(stdout, `"ok":false`) {
			t.Fatalf("rollback to an unknown version: err=%v stdout=%q", err, stdout)
		}
	})

	t.Run("logs and app exec", func(t *testing.T) {
		out, err := c.logs("--grep", "booted", "--lines", "5", "--role", "web", "--host", "alpha")
		if err != nil || !strings.Contains(out, "alpha|") || strings.Contains(out, "beta|") {
			t.Fatalf("logs = %q err=%v", out, err)
		}
		out, err = c.app("exec", "--role", "web", "migrate", "status")
		if err != nil || strings.TrimSpace(out) != "ran migrate status" {
			t.Fatalf("exec = %q err=%v", out, err)
		}
		out, err = c.app("exec", "--role", "jobs", "--reuse", "/app/server")
		if err != nil || !strings.Contains(out, "exec in shop-jobs-v1-") {
			t.Fatalf("exec --reuse = %q err=%v", out, err)
		}
	})

	t.Run("usage errors", func(t *testing.T) {
		for name, args := range map[string][]string{
			"deploy positional":     {"extra"},
			"setup positional":      {"setup", "extra"},
			"prune positional":      {"prune", "extra"},
			"rollback no version":   {"rollback"},
			"rollback two versions": {"rollback", "v1", "v2"},
			"details positional":    {"details", "extra"},
			"details dry run":       {"details", "--dry-run"},
			"lock without a verb":   {"lock"},
			"lock unknown verb":     {"lock", "freeze"},
			"maintenance no arg":    {"maintenance"},
			"maintenance bad arg":   {"maintenance", "sideways"},
			"unknown flag":          {"--nope"},
			"rollback unknown flag": {"rollback", "--nope"},
			"setup unknown flag":    {"setup", "--nope"},
			"details unknown flag":  {"details", "--nope"},
			"lock unknown flag":     {"lock", "status", "--nope"},
			"maintenance flag":      {"maintenance", "--nope"},
		} {
			if _, _, err := c.deploy(args...); err == nil {
				t.Errorf("%s: forge deploy %v succeeded", name, args)
			}
		}
		if _, err := c.app("shell", "extra"); err == nil {
			t.Error("app shell accepted a command")
		}
		if _, err := c.app("exec", "--nope"); err == nil {
			t.Error("app exec accepted an unknown flag")
		}
		if _, err := c.logs("--nope"); err == nil {
			t.Error("logs accepted an unknown flag")
		}
	})

	t.Run("a secrets file other users can read is refused", func(t *testing.T) {
		loose := filepath.Join(c.dir, "loose-secrets")
		if err := os.WriteFile(loose, []byte("REGISTRY_PASSWORD=whatever-secret\n"), 0o644); err != nil { // #nosec G306 -- the point of the test.
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		err := RunDeploy([]string{"details", "--config", c.config, "--secrets", loose}, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "accessible by other users") || strings.Contains(err.Error(), "whatever-secret") {
			t.Fatalf("err = %v", err)
		}
		for _, run := range []func() error{
			func() error { return RunLogs([]string{"--config", c.config, "--secrets", loose}, &out, &errOut) },
			func() error {
				return RunApp([]string{"exec", "--config", c.config, "--secrets", loose, "x"}, &out, &errOut)
			},
			func() error {
				return RunDeploy([]string{"lock", "status", "--config", c.config, "--secrets", loose}, &out, &errOut)
			},
		} {
			if err := run(); err == nil {
				t.Error("a command accepted loose secrets")
			}
		}
	})
}

func TestDefaultServiceNameFromDirectoryAndModule(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "My_Shop App")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := defaultServiceName(dir); got != "my-shop-app" {
		t.Errorf("directory name: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/9lives\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultServiceName(dir); got != "app-9lives" {
		t.Errorf("a name starting with a digit: %q", got)
	}
	long := strings.Repeat("x", 60)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/"+long+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultServiceName(dir); len(got) != 40 {
		t.Errorf("a long name: %q", got)
	}
}

func TestRunDeployInitRefusesExtraArguments(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, errOut bytes.Buffer
	if err := RunDeploy([]string{"init", "extra"}, &out, &errOut); err == nil {
		t.Error("init accepted a positional argument")
	}
	if err := RunDeploy([]string{"init", "--nope"}, &out, &errOut); err == nil {
		t.Error("init accepted an unknown flag")
	}
	if err := RunDeploy([]string{"init", "--service", "orders"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
}
