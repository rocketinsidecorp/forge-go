package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/credentials"
	"github.com/rocketinsidecorp/forge-go/encryption"
)

const (
	secretValue = "sk_live_VERY_SECRET_VALUE"
	helperEnv   = "FORGE_TEST_EDITOR_HELPER"
)

type credHarness struct {
	ctx            credentialsContext
	root           string
	stdout, stderr *bytes.Buffer
	env            map[string]string
}

func newCredHarness(t *testing.T) *credHarness {
	t.Helper()
	root := t.TempDir()
	h := &credHarness{root: root, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, env: map[string]string{}}
	h.ctx = credentialsContext{
		dir:          filepath.Join(root, "config", "credentials"),
		gitignore:    filepath.Join(root, ".gitignore"),
		dockerignore: filepath.Join(root, ".dockerignore"),
		lookup:       func(name string) (string, bool) { v, ok := h.env[name]; return v, ok },
		stdout:       h.stdout,
		stderr:       h.stderr,
		stdoutIsTTY:  func() bool { return false },
		editor:       runEditorProcess,
	}
	return h
}

func (h *credHarness) run(t *testing.T, args ...string) error {
	t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	return h.ctx.run(args)
}

func (h *credHarness) encPath(env string) string {
	return filepath.Join(h.ctx.dir, credentials.FileName(env))
}

func (h *credHarness) keyPath(env string) string {
	return filepath.Join(h.ctx.dir, credentials.KeyFileName(env))
}

// seed creates production credentials holding secretValue.
func (h *credHarness) seed(t *testing.T, env string) {
	t.Helper()
	if err := h.run(t, "init", "--environment", env); err != nil {
		t.Fatal(err)
	}
	keys, _, err := credentials.LoadKeys(h.ctx.dir, env, h.ctx.lookup)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := credentials.Seal(env, []byte(`{"stripe": {"api_key": "`+secretValue+`"}, "n": 7}`), keys[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteFile(h.ctx.dir, env, sealed, true); err != nil {
		t.Fatal(err)
	}
}

func (h *credHarness) read(t *testing.T, env string) *credentials.Store {
	t.Helper()
	store, err := credentials.Open(env, credentials.Options{Dir: h.ctx.dir, Lookup: h.ctx.lookup})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertNoSecret(t *testing.T, name string, outputs ...string) {
	t.Helper()
	for _, out := range outputs {
		if strings.Contains(out, secretValue) {
			t.Errorf("%s leaked the secret: %s", name, out)
		}
	}
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s still exists (err %v): plaintext was left behind", path, err)
	}
}

// TestCredentialsEditorHelper is the fake editor: the test binary re-executed
// as $EDITOR. It is a no-op in a normal test run.
func TestCredentialsEditorHelper(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process")
	}
	args := os.Args
	path := args[len(args)-1]
	record := os.Getenv("FORGE_TEST_EDITOR_RECORD")
	info, _ := os.Stat(path)
	dirInfo, _ := os.Stat(filepath.Dir(path))
	content, _ := os.ReadFile(path)
	entry, _ := json.Marshal(map[string]any{"path": path, "file_mode": info.Mode().Perm(), "dir_mode": dirInfo.Mode().Perm(), "content": string(content)})
	if err := os.WriteFile(record, entry, 0o600); err != nil {
		os.Exit(3)
	}
	// A swap file next to the edited file must be removed with it.
	_ = os.WriteFile(path+".swp", []byte("plaintext swap "+string(content)), 0o600)
	switch os.Getenv("FORGE_TEST_EDITOR_MODE") {
	case "write":
		if err := os.WriteFile(path, []byte(os.Getenv("FORGE_TEST_EDITOR_CONTENT")), 0o600); err != nil {
			os.Exit(3)
		}
	case "fail":
		os.Exit(1)
	case "sleep":
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

type editorRecord struct {
	Path     string      `json:"path"`
	FileMode os.FileMode `json:"file_mode"`
	DirMode  os.FileMode `json:"dir_mode"`
	Content  string      `json:"content"`
}

func (h *credHarness) useHelperEditor(t *testing.T, mode, content string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "record.json")
	t.Setenv(helperEnv, "1")
	t.Setenv("FORGE_TEST_EDITOR_MODE", mode)
	t.Setenv("FORGE_TEST_EDITOR_CONTENT", content)
	t.Setenv("FORGE_TEST_EDITOR_RECORD", record)
	h.env["EDITOR"] = exe + " -test.run=^TestCredentialsEditorHelper$ --"
	return record
}

func readRecord(t *testing.T, record string) editorRecord {
	t.Helper()
	var r editorRecord
	if err := json.Unmarshal(fileBytes(t, record), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCredentialsInitCreatesKeyFileAndIgnoreEntries(t *testing.T) {
	h := newCredHarness(t)
	if err := os.WriteFile(h.ctx.gitignore, []byte("/bin/\n.env.local"), 0o600); err != nil { // no trailing newline
		t.Fatal(err)
	}
	if err := os.WriteFile(h.ctx.dockerignore, []byte(".git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "init", "--environment", "production"); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(h.keyPath("production")); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("key file: %v %v; want 0600", info, err)
		}
	}
	key := strings.TrimSpace(string(fileBytes(t, h.keyPath("production"))))
	if _, err := encryption.ParseKey(key); err != nil {
		t.Fatalf("key file does not hold a valid key: %v", err)
	}
	if strings.Contains(h.stdout.String(), key) {
		t.Fatal("init printed the key")
	}
	store := h.read(t, "production")
	ring, err := store.KeyRing("encryption")
	if err != nil || ring.ActiveID() != "k1" {
		t.Fatalf("skeleton has no usable encryption key ring: %v", err)
	}
	if !bytes.Contains(fileBytes(t, h.encPath("production")), []byte(`"key_id"`)) {
		t.Fatal("not an envelope")
	}
	gitignore := string(fileBytes(t, h.ctx.gitignore))
	if !strings.Contains(gitignore, "\n"+gitignoreEntry+"\n") || !strings.HasPrefix(gitignore, "/bin/\n.env.local\n") {
		t.Fatalf(".gitignore = %q", gitignore)
	}
	if !strings.Contains(string(fileBytes(t, h.ctx.dockerignore)), dockerignoreEntry) {
		t.Fatal(".dockerignore not updated")
	}

	// A second init must neither replace the file nor the key.
	before, keyBefore := fileBytes(t, h.encPath("production")), fileBytes(t, h.keyPath("production"))
	if err := h.run(t, "init", "--environment", "production"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second init: %v", err)
	}
	if !bytes.Equal(before, fileBytes(t, h.encPath("production"))) || !bytes.Equal(keyBefore, fileBytes(t, h.keyPath("production"))) {
		t.Fatal("second init modified files")
	}
	if strings.Count(string(fileBytes(t, h.ctx.gitignore)), gitignoreEntry) != 1 {
		t.Fatal("gitignore entry duplicated")
	}
	// A restored key starts a new file instead of being replaced.
	if err := os.Remove(h.encPath("production")); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "init", "--environment", "production"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyBefore, fileBytes(t, h.keyPath("production"))) {
		t.Fatal("init replaced an existing key")
	}
}

func TestCredentialsInitCreatesGitignoreWhenMissing(t *testing.T) {
	h := newCredHarness(t)
	if err := h.run(t, "init"); err != nil { // default environment: development
		t.Fatal(err)
	}
	if got := string(fileBytes(t, h.ctx.gitignore)); !strings.Contains(got, gitignoreEntry) {
		t.Fatalf(".gitignore = %q", got)
	}
	if _, err := os.Stat(h.ctx.dockerignore); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("init must not create a .dockerignore")
	}
	h.env["FORGE_ENV"] = "staging"
	if err := h.run(t, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.encPath("staging")); err != nil {
		t.Fatalf("FORGE_ENV was not the default environment: %v", err)
	}
}

func TestCredentialsEditWithFakeEditor(t *testing.T) {
	h := newCredHarness(t)
	h.seed(t, "production")
	record := h.useHelperEditor(t, "write", `{"stripe": {"api_key": "sk_live_ROTATED"}, "added": "yes"}`)

	if err := h.run(t, "edit", "--environment", "production"); err != nil {
		t.Fatal(err)
	}
	r := readRecord(t, record)
	if runtime.GOOS != "windows" && (r.FileMode != 0o600 || r.DirMode != 0o700) {
		t.Fatalf("temporary file mode %04o, directory %04o; want 0600 and 0700", r.FileMode, r.DirMode)
	}
	if !strings.Contains(r.Content, secretValue) || !strings.Contains(r.Content, "\n  ") {
		t.Fatalf("editor received %q; want the decrypted, indented document", r.Content)
	}
	assertGone(t, r.Path)
	assertGone(t, r.Path+".swp")
	assertGone(t, filepath.Dir(r.Path))
	store := h.read(t, "production")
	if got, _ := store.String("stripe", "api_key"); got != "sk_live_ROTATED" {
		t.Fatalf("saved value = %q", got)
	}
	if got, _ := store.String("added"); got != "yes" {
		t.Fatal("new key not saved")
	}
	if bytes.Contains(fileBytes(t, h.encPath("production")), []byte("ROTATED")) {
		t.Fatal("plaintext in the encrypted file")
	}
	assertNoSecret(t, "edit output", h.stdout.String(), h.stderr.String())
}

func TestCredentialsEditCleansUpWhenEditorFails(t *testing.T) {
	h := newCredHarness(t)
	h.seed(t, "production")
	before := fileBytes(t, h.encPath("production"))
	record := h.useHelperEditor(t, "fail", "")
	err := h.run(t, "edit", "--environment", "production")
	if err == nil || !strings.Contains(err.Error(), "unchanged") {
		t.Fatalf("editor failure: %v", err)
	}
	r := readRecord(t, record)
	assertGone(t, r.Path)
	assertGone(t, r.Path+".swp")
	assertGone(t, filepath.Dir(r.Path))
	if !bytes.Equal(before, fileBytes(t, h.encPath("production"))) {
		t.Fatal("credentials changed although the editor failed")
	}
	assertNoSecret(t, "error", err.Error(), h.stdout.String(), h.stderr.String())
}

func TestCredentialsEditRejectsInvalidDocuments(t *testing.T) {
	cases := map[string]string{
		"syntax":    "{\n  \"a\": \"" + secretValue + "\" \"b\": 1\n}",
		"duplicate": `{"a": 1, "a": 2}`,
		"array":     `["` + secretValue + `"]`,
		"empty":     ``,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			h := newCredHarness(t)
			h.seed(t, "production")
			before := fileBytes(t, h.encPath("production"))
			record := h.useHelperEditor(t, "write", content)
			err := h.run(t, "edit", "--environment", "production")
			if err == nil || !strings.Contains(err.Error(), "unchanged") {
				t.Fatalf("invalid document accepted: %v", err)
			}
			assertNoSecret(t, "error", err.Error(), h.stdout.String(), h.stderr.String())
			assertGone(t, readRecord(t, record).Path)
			if !bytes.Equal(before, fileBytes(t, h.encPath("production"))) {
				t.Fatal("credentials changed")
			}
		})
	}
}

func TestCredentialsEditWithoutChangesDoesNotRewrite(t *testing.T) {
	h := newCredHarness(t)
	h.seed(t, "production")
	before := fileBytes(t, h.encPath("production"))
	h.useHelperEditor(t, "none", "")
	if err := h.run(t, "edit", "--environment", "production"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.stdout.String(), "no changes") || !bytes.Equal(before, fileBytes(t, h.encPath("production"))) {
		t.Fatalf("file was rewritten without changes: %q", h.stdout.String())
	}
}

func TestCredentialsEditCleansUpOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals")
	}
	h := newCredHarness(t)
	h.seed(t, "production")
	before := fileBytes(t, h.encPath("production"))
	record := h.useHelperEditor(t, "sleep", "")
	done := make(chan error, 1)
	go func() { done <- h.run(t, "edit", "--environment", "production") }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(record); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the editor never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The helper writes the record before it sleeps; give it a moment to
	// reach the sleep, then terminate the forge process (this one).
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("edit did not stop on SIGTERM")
	}
	r := readRecord(t, record)
	assertGone(t, r.Path)
	assertGone(t, r.Path+".swp")
	assertGone(t, filepath.Dir(r.Path))
	if !bytes.Equal(before, fileBytes(t, h.encPath("production"))) {
		t.Fatal("credentials changed after an interrupted edit")
	}
}

func TestCredentialsEditRefusesToOverwriteConcurrentChange(t *testing.T) {
	h := newCredHarness(t)
	h.seed(t, "production")
	h.env["EDITOR"] = "fake"
	var tempPath string
	h.ctx.editor = func(_ []string, path string) error {
		tempPath = path
		// Somebody else re-encrypts the file while the editor is open.
		keys, _, _ := credentials.LoadKeys(h.ctx.dir, "production", h.ctx.lookup)
		sealed, _ := credentials.Seal("production", []byte(`{"theirs": "1"}`), keys[0])
		if err := credentials.WriteFile(h.ctx.dir, "production", sealed, true); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(`{"mine": "1"}`), 0o600)
	}
	err := h.run(t, "edit", "--environment", "production")
	if err == nil || !strings.Contains(err.Error(), "changed while you were editing") {
		t.Fatalf("concurrent change: %v", err)
	}
	if got, _ := h.read(t, "production").String("theirs"); got != "1" {
		t.Fatal("the other change was overwritten")
	}
	assertGone(t, tempPath)
}

func TestCredentialsEditNeedsEditorKeyAndFile(t *testing.T) {
	h := newCredHarness(t)
	if err := h.run(t, "edit", "--environment", "production"); err == nil || !strings.Contains(err.Error(), "no editor") {
		t.Fatalf("no editor: %v", err)
	}
	h.env["EDITOR"] = "true"
	if err := h.run(t, "edit", "--environment", "production"); err == nil || !strings.Contains(err.Error(), "init") {
		t.Fatalf("no key or file: %v", err)
	}
	h.seed(t, "production")
	if err := os.Remove(h.keyPath("production")); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "edit", "--environment", "production"); !errors.Is(err, credentials.ErrKeyMissing) {
		t.Fatalf("missing key: %v", err)
	}
}

func TestEditorCommandHardeningAndPrecedence(t *testing.T) {
	h := newCredHarness(t)
	h.env["EDITOR"] = "nano"
	h.env["VISUAL"] = "/usr/bin/vim --noplugin"
	got := h.ctx.editorCommand()
	if got[0] != "/usr/bin/vim" || got[1] != "--noplugin" || !strings.Contains(strings.Join(got, " "), "-n -i NONE") {
		t.Fatalf("VISUAL must win and vim must run without swap/viminfo: %v", got)
	}
	delete(h.env, "VISUAL")
	if got := h.ctx.editorCommand(); len(got) != 1 || got[0] != "nano" {
		t.Fatalf("EDITOR = %v", got)
	}
}

func TestCredentialsShowRedactsByDefaultAndRevealNeedsTerminal(t *testing.T) {
	h := newCredHarness(t)
	h.seed(t, "production")
	if err := h.run(t, "show", "--environment", "production"); err != nil {
		t.Fatal(err)
	}
	out := h.stdout.String()
	assertNoSecret(t, "redacted show", out)
	var shown map[string]any
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatalf("show output is not JSON: %v\n%s", err, out)
	}
	if shown["n"] != redactedValue || shown["stripe"].(map[string]any)["api_key"] != redactedValue {
		t.Fatalf("values not redacted: %s", out)
	}
	if err := h.run(t, "show", "--environment", "production", "--reveal"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("--reveal without a terminal: %v", err)
	}
	assertNoSecret(t, "refused reveal", h.stdout.String())
	h.ctx.stdoutIsTTY = func() bool { return true }
	if err := h.run(t, "show", "--environment", "production", "--reveal"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.stdout.String(), secretValue) {
		t.Fatalf("--reveal on a terminal did not print the value: %q", h.stdout.String())
	}
}

func TestCredentialsShowFailsClosed(t *testing.T) {
	h := newCredHarness(t)
	h.seed(t, "production")
	h.env[credentials.EnvMasterKey] = encryption.EncodeKey(bytes.Repeat([]byte{9}, 32))
	err := h.run(t, "show", "--environment", "production")
	if err == nil || h.stdout.Len() != 0 {
		t.Fatalf("show with the wrong key: %v / %q", err, h.stdout.String())
	}
	assertNoSecret(t, "error", err.Error())
	// A staging file installed as production's.
	delete(h.env, credentials.EnvMasterKey)
	h.seed(t, "staging")
	if err := os.WriteFile(h.encPath("production"), fileBytes(t, h.encPath("staging")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "show", "--environment", "production"); err == nil {
		t.Fatal("a file from another environment was accepted")
	}
}

func TestCredentialsRotateKeyWithKeyFile(t *testing.T) {
	h := newCredHarness(t)
	h.seed(t, "production")
	oldKeys, _, err := credentials.LoadKeys(h.ctx.dir, "production", h.ctx.lookup)
	if err != nil {
		t.Fatal(err)
	}
	oldFile := fileBytes(t, h.encPath("production"))
	if err := h.run(t, "rotate-key", "--environment", "production"); err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "rotate output", h.stdout.String(), h.stderr.String())
	if bytes.Contains([]byte(h.stdout.String()+h.stderr.String()), []byte(encryption.EncodeKey(oldKeys[0].Material))) {
		t.Fatal("rotate printed the old key")
	}
	if h.stdout.Len() != 0 {
		t.Fatalf("the new key must not be printed unless asked: %q", h.stdout.String())
	}
	newFile := fileBytes(t, h.encPath("production"))
	if bytes.Equal(oldFile, newFile) {
		t.Fatal("file not re-encrypted")
	}
	if got, _ := h.read(t, "production").String("stripe", "api_key"); got != secretValue {
		t.Fatalf("value after rotation = %q", got)
	}
	if _, err := credentials.Unseal("production", newFile, oldKeys); !errors.Is(err, credentials.ErrNoMatchingKey) {
		t.Fatalf("the old key still works: %v", err)
	}
	if info, err := os.Stat(h.keyPath("production")); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("key file mode: %v %v", info, err)
	}
	if n := strings.Count(strings.TrimSpace(string(fileBytes(t, h.keyPath("production")))), "\n") + 1; n != 1 {
		t.Fatalf("key file holds %d keys, want only the new one", n)
	}
}

func TestCredentialsRotateKeyKeepOld(t *testing.T) {
	h := newCredHarness(t)
	h.seed(t, "production")
	oldKeys, _, _ := credentials.LoadKeys(h.ctx.dir, "production", h.ctx.lookup)
	oldFile := fileBytes(t, h.encPath("production"))
	if err := h.run(t, "rotate-key", "--environment", "production", "--keep-old", "--print-key"); err != nil {
		t.Fatal(err)
	}
	printed, err := credentials.ParseKeys(strings.TrimSpace(h.stdout.String()))
	if err != nil || len(printed) != 2 || printed[1].ID != oldKeys[0].ID {
		t.Fatalf("printed key list: %v %v", printed, err)
	}
	// The migration window: an old-key-encrypted file stays readable.
	if plain, err := credentials.Unseal("production", oldFile, printed); err != nil || !strings.Contains(string(plain), secretValue) {
		t.Fatalf("old file with the kept key: %v", err)
	}
	keys, _, _ := credentials.LoadKeys(h.ctx.dir, "production", h.ctx.lookup)
	if len(keys) != 2 || keys[0].ID != printed[0].ID {
		t.Fatalf("key file keys: %v", keys)
	}
}

func TestCredentialsRotateKeyFromEnvironmentKey(t *testing.T) {
	h := newCredHarness(t)
	h.seed(t, "production")
	keys, _, _ := credentials.LoadKeys(h.ctx.dir, "production", h.ctx.lookup)
	if err := os.Remove(h.keyPath("production")); err != nil { // production machines have no key file
		t.Fatal(err)
	}
	h.env[credentials.EnvMasterKey] = encryption.EncodeKey(keys[0].Material)
	before := fileBytes(t, h.encPath("production"))
	if err := h.run(t, "rotate-key", "--environment", "production"); err == nil || !strings.Contains(err.Error(), "--print-key") {
		t.Fatalf("rotating without anywhere to store the new key: %v", err)
	}
	if !bytes.Equal(before, fileBytes(t, h.encPath("production"))) {
		t.Fatal("file changed although the new key had nowhere to go")
	}
	if _, err := os.Stat(h.keyPath("production")); err == nil {
		t.Fatal("a key file was created without --write-key-file")
	}
	if err := h.run(t, "rotate-key", "--environment", "production", "--print-key"); err != nil {
		t.Fatal(err)
	}
	h.env[credentials.EnvMasterKey] = strings.TrimSpace(h.stdout.String())
	if got, _ := h.read(t, "production").String("stripe", "api_key"); got != secretValue {
		t.Fatalf("value with the printed key = %q", got)
	}
	if _, err := os.Stat(h.keyPath("production")); err == nil {
		t.Fatal("a key file appeared")
	}
}

func TestCredentialsCommandValidation(t *testing.T) {
	h := newCredHarness(t)
	for _, args := range [][]string{
		nil, {"bogus"}, {"edit", "--environment", "../x"}, {"show", "--environment", "Prod"},
		{"init", "extra"}, {"show", "--keep-old"}, {"edit", "--reveal"},
	} {
		if err := h.run(t, args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestCredentialsRotateKeyChangedFileLeavesKeyFileAlone(t *testing.T) {
	h := newCredHarness(t)
	h.seed(t, "production")
	keyBefore := fileBytes(t, h.keyPath("production"))
	h.ctx.afterRead = func() {
		keys, _, _ := credentials.LoadKeys(h.ctx.dir, "production", h.ctx.lookup)
		sealed, _ := credentials.Seal("production", []byte(`{"theirs": "1"}`), keys[0])
		if err := credentials.WriteFile(h.ctx.dir, "production", sealed, true); err != nil {
			t.Fatal(err)
		}
	}
	err := h.run(t, "rotate-key", "--environment", "production", "--keep-old", "--print-key")
	if err == nil || !strings.Contains(err.Error(), "changed during rotation") {
		t.Fatalf("concurrent change: %v", err)
	}
	if !bytes.Equal(keyBefore, fileBytes(t, h.keyPath("production"))) {
		t.Fatal("the key file gained a new key although the rotation was refused")
	}
	if h.stdout.Len() != 0 {
		t.Fatalf("a key was printed although the rotation was refused: %q", h.stdout.String())
	}
	if got, _ := h.read(t, "production").String("theirs"); got != "1" {
		t.Fatal("the other change was overwritten")
	}
}
