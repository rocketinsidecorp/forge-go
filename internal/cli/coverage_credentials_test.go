package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/credentials"
)

// sealPlaintext replaces the credentials of env with plaintext, sealed with
// the environment's key.
func (h *credHarness) sealPlaintext(t *testing.T, env string, plaintext []byte) {
	t.Helper()
	keys, _, err := credentials.LoadKeys(h.ctx.dir, env, h.ctx.lookup)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := credentials.Seal(env, plaintext, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteFile(h.ctx.dir, env, sealed, true); err != nil {
		t.Fatal(err)
	}
}

func TestRunCredentialsUsesTheWorkingDirectory(t *testing.T) {
	// Not parallel: the working directory.
	t.Chdir(t.TempDir())
	t.Setenv("FORGE_ENV", "")
	var out, errOut bytes.Buffer
	if err := runCredentials([]string{"init"}, &out, &errOut); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, want := range []string{"config/credentials/development.json.enc", "config/credentials/development.key"} {
		if _, err := os.Stat(filepath.FromSlash(want)); err != nil {
			t.Errorf("%s: %v", want, err)
		}
	}
	out.Reset()
	if err := runCredentials([]string{"show"}, &out, &errOut); err != nil || !strings.Contains(out.String(), redactedValue) {
		t.Errorf("show: %v, %q", err, out.String())
	}
	// Revealing is for terminals; a buffer is not one.
	if err := runCredentials([]string{"show", "--reveal"}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("show --reveal into a buffer: %v", err)
	}
	if err := runCredentials([]string{"init"}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("a second init: %v", err)
	}
}

func TestCredentialsInitReportsWhatItCannotWrite(t *testing.T) {
	t.Parallel()
	t.Run("an ignore file that is a directory", func(t *testing.T) {
		t.Parallel()
		h := newCredHarness(t)
		if err := os.Mkdir(h.ctx.gitignore, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := h.run(t, "init"); err == nil || !strings.Contains(err.Error(), "update .gitignore") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a docker ignore file that is a directory", func(t *testing.T) {
		t.Parallel()
		h := newCredHarness(t)
		if err := os.Mkdir(h.ctx.dockerignore, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := h.run(t, "init"); err == nil || !strings.Contains(err.Error(), "update .dockerignore") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a credentials file that cannot be read", func(t *testing.T) {
		t.Parallel()
		h := newCredHarness(t)
		if err := os.MkdirAll(h.encPath("development"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := h.run(t, "init"); err == nil || strings.Contains(err.Error(), "already exists") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a credentials directory that is a file", func(t *testing.T) {
		t.Parallel()
		h := newCredHarness(t)
		if err := os.MkdirAll(filepath.Dir(h.ctx.dir), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.ctx.dir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.run(t, "init"); err == nil {
			t.Error("init below a file must fail")
		}
	})
}

func TestCredentialsShowAndEditFailClosedOnUnreadableState(t *testing.T) {
	t.Parallel()
	h := newCredHarness(t)
	h.seed(t, "production")
	// A key that is not a key.
	h.env[credentials.EnvMasterKey] = "not-a-key"
	for _, args := range [][]string{{"show", "-e", "production"}, {"rotate-key", "-e", "production", "--print-key"}} {
		if err := h.run(t, args...); err == nil {
			t.Errorf("%v with an invalid key succeeded", args)
		}
	}
	delete(h.env, credentials.EnvMasterKey)
	// The encrypted file is a directory.
	if err := os.Remove(h.encPath("production")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.encPath("production"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "show", "-e", "production"); err == nil {
		t.Error("show of an unreadable file succeeded")
	}
	// The encrypted file is missing: the message names the fix.
	if err := os.RemoveAll(h.encPath("production")); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "show", "-e", "production"); err == nil || !strings.Contains(err.Error(), "forge credentials init --environment production") {
		t.Errorf("a missing file: %v", err)
	}
}

// redactDocument parses what it is given: the stored document is valid JSON
// (Seal refuses anything else), but the function does not rely on that.
func TestCredentialsShowRedactsEveryValueKind(t *testing.T) {
	t.Parallel()
	h := newCredHarness(t)
	h.seed(t, "production")
	h.sealPlaintext(t, "production", []byte(`{"list": [1, "two", {"three": "secret-three"}], "nested": {"n": 2.5, "b": true, "z": null}}`))
	if err := h.run(t, "show", "-e", "production"); err != nil {
		t.Fatal(err)
	}
	out := h.stdout.String()
	if strings.Contains(out, "secret-three") || strings.Contains(out, "2.5") || strings.Count(out, redactedValue) != 6 {
		t.Errorf("redacted output:\n%s", out)
	}
	for _, key := range []string{`"list"`, `"three"`, `"nested"`} {
		if !strings.Contains(out, key) {
			t.Errorf("the structure lost %s:\n%s", key, out)
		}
	}
	if _, err := redactDocument([]byte("{")); err == nil {
		t.Error("redactDocument accepted broken JSON")
	}
}

func TestCredentialsRotateKeyReportsWhatItCannotWrite(t *testing.T) {
	t.Parallel()
	t.Run("the key list cannot be printed", func(t *testing.T) {
		t.Parallel()
		h := newCredHarness(t)
		h.seed(t, "production")
		before := fileBytes(t, h.encPath("production"))
		h.ctx.stdout = failingWriter{}
		if err := h.run(t, "rotate-key", "-e", "production", "--print-key"); err == nil || err != errBrokenPipe {
			t.Errorf("err = %v", err)
		}
		if !bytes.Equal(before, fileBytes(t, h.encPath("production"))) {
			t.Error("a failed print must not change the credentials")
		}
	})
	t.Run("the new key cannot be stored", func(t *testing.T) {
		t.Parallel()
		h := newCredHarness(t)
		h.seed(t, "production")
		before := fileBytes(t, h.encPath("production"))
		// The key arrives through the environment; the key file path is taken
		// by a directory, so --write-key-file cannot create it.
		h.env[credentials.EnvMasterKey] = strings.TrimSpace(string(fileBytes(t, h.keyPath("production"))))
		if err := os.Remove(h.keyPath("production")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(h.keyPath("production"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := h.run(t, "rotate-key", "-e", "production", "--write-key-file"); err == nil {
			t.Error("rotation without a writable key file succeeded")
		}
		if !bytes.Equal(before, fileBytes(t, h.encPath("production"))) {
			t.Error("a failed rotation changed the credentials")
		}
	})
}

// --- editing ---

func (h *credHarness) fakeEditor(fn func(path string) error) {
	h.env["EDITOR"] = "fake-editor"
	h.ctx.editor = func(_ []string, path string) error { return fn(path) }
}

func TestCredentialsEditRefusesDocumentsItCannotHandle(t *testing.T) {
	t.Parallel()
	t.Run("the editor leaves nothing to read", func(t *testing.T) {
		t.Parallel()
		h := newCredHarness(t)
		h.seed(t, "production")
		before := fileBytes(t, h.encPath("production"))
		h.fakeEditor(func(path string) error { return os.Remove(path) })
		if err := h.run(t, "edit", "-e", "production"); err == nil {
			t.Error("edit with a deleted file succeeded")
		}
		if !bytes.Equal(before, fileBytes(t, h.encPath("production"))) {
			t.Error("credentials changed")
		}
	})
	t.Run("the edited document is too large", func(t *testing.T) {
		t.Parallel()
		h := newCredHarness(t)
		h.seed(t, "production")
		h.fakeEditor(func(path string) error {
			return os.WriteFile(path, bytes.Repeat([]byte(" "), credentials.MaxPlaintextBytes+10), 0o600)
		})
		if err := h.run(t, "edit", "-e", "production"); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("the editor cannot be started", func(t *testing.T) {
		t.Parallel()
		h := newCredHarness(t)
		h.seed(t, "production")
		h.env["EDITOR"] = filepath.Join(h.root, "no-such-editor")
		if err := h.run(t, "edit", "-e", "production"); err == nil || !strings.Contains(err.Error(), "editor failed; credentials are unchanged") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestCredentialsEditLeavesAnInterruptTypedInTheEditorToTheEditor(t *testing.T) {
	// Not parallel: it signals the test process.
	h := newCredHarness(t)
	h.seed(t, "production")
	dir := t.TempDir()
	started, proceed := filepath.Join(dir, "started"), filepath.Join(dir, "proceed")
	script := filepath.Join(dir, "editor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n: > '"+started+"'\nwhile [ ! -e '"+proceed+"' ]; do sleep 0.02; done\n"), 0o700); err != nil { // #nosec G306 -- test editor must be executable
		t.Fatal(err)
	}
	h.env["EDITOR"] = script
	done := make(chan error, 1)
	go func() { done <- h.ctx.run([]string{"edit", "-e", "production"}) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the editor never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The terminal delivers Ctrl-C to forge and to the editor.
	if err := signalSelf(syscall.SIGINT); err != nil { // returns once both forge handlers have it
		t.Fatal(err)
	}
	if err := os.WriteFile(proceed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("edit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("edit did not finish")
	}
	if !strings.Contains(h.stdout.String(), "no changes") {
		t.Errorf("output %q", h.stdout.String())
	}
}

func TestShredFileAndReadBoundedReportMissingFiles(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	if err := shredFile(missing); err == nil {
		t.Error("shredFile of a missing file succeeded")
	}
	if _, err := readBounded(missing, 10); err == nil {
		t.Error("readBounded of a missing file succeeded")
	}
	dir := t.TempDir()
	if _, err := readBounded(dir, 10); err == nil {
		t.Error("readBounded of a directory succeeded")
	}
	file := filepath.Join(dir, "secret")
	if err := os.WriteFile(file, []byte("sensitive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := shredDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("shredDir left the directory: %v", err)
	}
}
