//go:build unix

package deploy

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/exectest"
)

// A failing remote command whose stderr carries a secret must not leak it
// through the returned error, on any path that reaches the runner.
func TestRemoteErrorsAreRedactedOnEveryRunnerPath(t *testing.T) {
	const secret = "postgres://shop:leaky-password-42@db/shop"
	dir := t.TempDir()
	fakeSSH := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\necho 'connect failed: " + secret + "' >&2\nexit 3\n"
	if err := exectest.Write(fakeSSH, script); err != nil {
		t.Fatal(err)
	}
	d, _, _, _ := planFixture(t, false)
	d.Secrets = NewSecrets(map[string]string{"REGISTRY_PASSWORD": "registry-secret-1", "FORGE_DATABASE_URL": secret, "POSTGRES_PASSWORD": "pg-secret-3"})
	runner := &SSHRunner{Binary: fakeSSH}
	t.Cleanup(func() { _ = runner.Close() })
	d.Runner = runner

	var out bytes.Buffer
	checks := map[string]error{
		"exec":         d.Exec(t.Context(), &out, ExecOptions{Argv: []string{"migrate", "status"}}),
		"exec --reuse": d.Exec(t.Context(), &out, ExecOptions{Reuse: true, Argv: []string{"/app/server"}}),
		"logs":         d.Logs(t.Context(), &out, LogsOptions{}),
		"logs follow":  d.Logs(t.Context(), &out, LogsOptions{Follow: true}),
		"lock status": func() error {
			_, err := d.LockStatus(t.Context())
			return err
		}(),
	}
	for name, err := range checks {
		if err == nil {
			t.Errorf("%s: expected an error from the failing remote command", name)
			continue
		}
		if strings.Contains(err.Error(), "leaky-password") {
			t.Errorf("%s: error leaks the secret: %v", name, err)
		}
		if !strings.Contains(err.Error(), Redacted) {
			t.Errorf("%s: error lost the redacted diagnostic: %v", name, err)
		}
	}
	if strings.Contains(out.String(), "leaky-password") {
		t.Fatalf("streamed output leaks the secret: %s", out.String())
	}
}
