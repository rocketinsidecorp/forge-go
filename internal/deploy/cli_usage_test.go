package deploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// The commands refuse malformed command lines before they read a
// configuration or open a connection.
func TestDeployCommandsRefuseMalformedCommandLinesBeforeConnecting(t *testing.T) {
	// Not parallel: the working directory has no deploy configuration, and the
	// commands must fail on usage, not on that.
	t.Chdir(t.TempDir())
	cases := map[string]struct {
		run  func(args []string, stdout, stderr *bytes.Buffer) error
		args []string
		want string
	}{
		"deploy with an operand":        {runDeploy, []string{"--version", "v1", "stray"}, "usage: forge deploy"},
		"deploy with an unknown flag":   {runDeploy, []string{"--nope"}, "flag provided but not defined"},
		"setup with an operand":         {runDeploy, []string{"setup", "stray"}, "usage: forge deploy setup"},
		"prune with an operand":         {runDeploy, []string{"prune", "stray"}, "usage: forge deploy prune"},
		"rollback without a version":    {runDeploy, []string{"rollback"}, "usage: forge deploy rollback"},
		"rollback with two versions":    {runDeploy, []string{"rollback", "v1", "v2"}, "usage: forge deploy rollback"},
		"details with an operand":       {runDeploy, []string{"details", "stray"}, "usage: forge deploy details"},
		"details as a dry run":          {runDeploy, []string{"details", "--dry-run"}, "usage: forge deploy details"},
		"lock without an action":        {runDeploy, []string{"lock"}, "usage: forge deploy lock"},
		"lock with an unknown action":   {runDeploy, []string{"lock", "frobnicate", "--config", "missing.yml"}, ""},
		"maintenance without a state":   {runDeploy, []string{"maintenance"}, "usage: forge deploy maintenance"},
		"maintenance with a bad state":  {runDeploy, []string{"maintenance", "sideways"}, "usage: forge deploy maintenance"},
		"an unknown subcommand":         {runDeploy, []string{"fly"}, `unknown deploy command "fly"`},
		"logs with an operand":          {runLogs, []string{"stray"}, "usage: forge logs"},
		"logs with an unknown flag":     {runLogs, []string{"--nope"}, "flag provided but not defined"},
		"app without a subcommand":      {runApp, nil, "usage: forge app"},
		"app with an unknown command":   {runApp, []string{"debug"}, "usage: forge app"},
		"app exec with an unknown flag": {runApp, []string{"exec", "--nope"}, "flag provided but not defined"},
	}
	for name, tc := range cases {
		var stdout, stderr bytes.Buffer
		err := tc.run(tc.args, &stdout, &stderr)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if tc.want != "" && !strings.Contains(err.Error()+stderr.String(), tc.want) {
			t.Errorf("%s: err = %v / stderr %q, want %q", name, err, stderr.String(), tc.want)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: printed %q before failing", name, stdout.String())
		}
	}
}

func runDeploy(args []string, stdout, stderr *bytes.Buffer) error {
	return RunDeploy(args, stdout, stderr)
}
func runLogs(args []string, stdout, stderr *bytes.Buffer) error { return RunLogs(args, stdout, stderr) }
func runApp(args []string, stdout, stderr *bytes.Buffer) error  { return RunApp(args, stdout, stderr) }

// A command that cannot load its configuration or its secrets says so.
func TestDeployCommandsReportAMissingConfigurationOrSecretsFile(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	config := filepath.Join(dir, "deploy.yml")
	if err := os.WriteFile(config, []byte(integrationConfigForUnit), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "no-secrets")
	for name, args := range map[string][]string{
		"no configuration":  {"setup", "--config", filepath.Join(dir, "absent.yml")},
		"no secrets":        {"setup", "--config", config, "--secrets", missing},
		"deploy, no config": {"--config", filepath.Join(dir, "absent.yml"), "--version", "v1"},
		"details":           {"details", "--config", filepath.Join(dir, "absent.yml")},
		"rollback":          {"rollback", "v1", "--config", filepath.Join(dir, "absent.yml")},
		"lock status":       {"lock", "status", "--config", filepath.Join(dir, "absent.yml")},
		"maintenance":       {"maintenance", "on", "--config", filepath.Join(dir, "absent.yml")},
	} {
		var stdout, stderr bytes.Buffer
		if err := RunDeploy(args, &stdout, &stderr); err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
	var stdout, stderr bytes.Buffer
	if err := RunLogs([]string{"--config", filepath.Join(dir, "absent.yml")}, &stdout, &stderr); err == nil {
		t.Error("logs without a configuration succeeded")
	}
	if err := RunApp([]string{"shell", "--config", filepath.Join(dir, "absent.yml")}, &stdout, &stderr); err == nil {
		t.Error("app shell without a configuration succeeded")
	}
}

const integrationConfigForUnit = `
service: shop
image: registry.example.com/shop
registry:
  server: registry.example.com
  username: ci
ssh:
  user: deploy
servers:
  web:
    - 203.0.113.10
`

func TestFinishReportPrintsWarningsFailuresAndEncodingErrors(t *testing.T) {
	t.Parallel()
	report := &Report{Action: "deploy", Service: "shop", Warnings: []string{"old image kept"}, DurationMS: 1500}

	var out bytes.Buffer
	if err := finishReport(&commonFlags{}, &out, report, nil, "deployed v1"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "warning: old image kept") || !strings.Contains(got, "deployed v1 (1.5s)") {
		t.Errorf("output = %q", got)
	}
	out.Reset()
	if err := finishReport(&commonFlags{dryRun: true}, &out, report, nil, "deployed v1"); err != nil || !strings.Contains(out.String(), "dry run: nothing was changed") {
		t.Errorf("dry run: %v %q", err, out.String())
	}

	// The JSON report of a failure still goes out, with the exit code.
	failure := &ExitError{Code: 3, Err: errors.New("host unreachable")}
	out.Reset()
	if err := finishReport(&commonFlags{json: true}, &out, report, failure, "x"); !errors.Is(err, failure) {
		t.Fatalf("err = %v, want the failure passed through", err)
	}
	var decoded struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded.OK || decoded.ExitCode != 3 || !strings.Contains(decoded.Error, "host unreachable") {
		t.Errorf("report = %+v (%v) from %q", decoded, err, out.String())
	}

	// A report that cannot be written is an error when nothing else failed,
	// and never hides the failure that did happen.
	if err := finishReport(&commonFlags{json: true}, brokenWriter{}, report, nil, "x"); err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Errorf("an unwritable report: %v", err)
	}
	if err := finishReport(&commonFlags{json: true}, brokenWriter{}, report, failure, "x"); !errors.Is(err, failure) {
		t.Errorf("an unwritable report hid the failure: %v", err)
	}
	out.Reset()
	if err := finishReport(&commonFlags{}, &out, report, failure, "x"); !errors.Is(err, failure) {
		t.Errorf("a failure without --json: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "warning: old image kept") || strings.Contains(got, "(1.5s)") {
		t.Errorf("a failure prints its warnings and no summary, got %q", got)
	}
	if err := finishReport(&commonFlags{}, &out, nil, nil, "x"); err != nil {
		t.Errorf("no report, no error: %v", err)
	}
}
