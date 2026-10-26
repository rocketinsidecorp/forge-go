package credentials_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/credentials"
	"github.com/rocketinsidecorp/forge-go/encryption"
)

func TestReadFileRefusesWhatIsNotAnEncryptedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := credentials.ReadFile(dir, "Production"); !errors.Is(err, credentials.ErrInvalidEnvironment) {
		t.Errorf("an invalid environment: %v", err)
	}
	if _, err := credentials.ReadFile(filepath.Join(dir, "absent"), "production"); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("a missing directory: %v", err)
	}
	if _, err := credentials.ReadFile(dir, "production"); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("a missing file: %v", err)
	}
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.ReadFile(file, "production"); err == nil || errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("a regular file as the directory: %v", err)
	}
	huge := bytes.Repeat([]byte{'x'}, credentials.MaxFileBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "production.json.enc"), huge, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.ReadFile(dir, "production"); !errors.Is(err, credentials.ErrFormat) {
		t.Errorf("an oversized file: %v", err)
	}
}

func TestReadFileAndWriteFileDefaultToTheCredentialsDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := credentials.WriteFile("", "production", []byte("contents"), false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(credentials.DefaultDir, "production.json.enc"))
	if err != nil || string(got) != "contents" {
		t.Fatalf("WriteFile with no directory wrote %q, %v", got, err)
	}
	if got, err := credentials.ReadFile("", "production"); err != nil || string(got) != "contents" {
		t.Fatalf("ReadFile with no directory = %q, %v", got, err)
	}
	// An empty directory means the default for keys too.
	key := newKey(t, 6)
	if err := credentials.WriteKeyFile("", "production", []encryption.Key{key}, false); err != nil {
		t.Fatal(err)
	}
	if err := credentials.PrepareKeyDir(""); err != nil {
		t.Fatal(err)
	}
	t.Setenv(credentials.EnvMasterKey, "")
	if keys, _, err := credentials.LoadKeys("", "production", nil); err != nil || len(keys) != 1 || keys[0].ID != key.ID {
		t.Fatalf("LoadKeys with no directory = %v, %v", keys, err)
	}
}

func TestWritesRefuseBadInputAndLeaveNothingBehind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key := newKey(t, 2)
	if err := credentials.WriteFile(dir, "Production", []byte("x"), true); !errors.Is(err, credentials.ErrInvalidEnvironment) {
		t.Errorf("WriteFile with an invalid environment: %v", err)
	}
	if err := credentials.WriteKeyFile(dir, "Production", []encryption.Key{key}, true); !errors.Is(err, credentials.ErrInvalidEnvironment) {
		t.Errorf("WriteKeyFile with an invalid environment: %v", err)
	}
	if err := credentials.WriteKeyFile(dir, "production", nil, true); err == nil {
		t.Error("WriteKeyFile accepted no keys")
	}
	tooMany := make([]encryption.Key, credentials.MaxKeys+1)
	for i := range tooMany {
		tooMany[i] = key
	}
	if err := credentials.WriteKeyFile(dir, "production", tooMany, true); err == nil {
		t.Error("WriteKeyFile accepted more than MaxKeys keys")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("refused writes left %d entries", len(entries))
	}

	// Without replace an existing file is never overwritten, and with one the
	// directory is not left holding the temporary file.
	if err := credentials.WriteFile(dir, "production", []byte("first"), false); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteFile(dir, "production", []byte("second"), false); !errors.Is(err, fs.ErrExist) {
		t.Errorf("WriteFile over an existing file: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "production.json.enc")); string(got) != "first" {
		t.Errorf("the file was overwritten: %q", got)
	}

	// A destination that cannot be replaced (here a directory holding a file)
	// fails the rename; the temporary file is removed.
	blocked := t.TempDir()
	target := filepath.Join(blocked, "staging.json.enc")
	if err := os.MkdirAll(filepath.Join(target, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteFile(blocked, "staging", []byte("x"), true); err == nil {
		t.Error("WriteFile replaced a directory")
	}
	entries, _ := os.ReadDir(blocked)
	if len(entries) != 1 || entries[0].Name() != "staging.json.enc" {
		t.Errorf("a failed write left %v", entries)
	}
}

func TestKeyDirectoriesCannotBeCreatedUnderAFile(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	below := filepath.Join(file, "credentials")
	key := newKey(t, 3)
	if err := credentials.PrepareKeyDir(below); err == nil {
		t.Error("PrepareKeyDir below a file succeeded")
	}
	if err := credentials.WriteKeyFile(below, "production", []encryption.Key{key}, false); err == nil {
		t.Error("WriteKeyFile below a file succeeded")
	}
	if err := credentials.WriteFile(below, "production", []byte("x"), false); err == nil {
		t.Error("WriteFile below a file succeeded")
	}
}

func TestPrepareKeyDirCreatesAPrivateDirectory(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "a", "credentials")
	if err := credentials.PrepareKeyDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || (enforcesModes && info.Mode().Perm()&0o077 != 0) {
		t.Fatalf("PrepareKeyDir created %v, %v", info, err)
	}
	if err := credentials.PrepareKeyDir(dir); err != nil {
		t.Errorf("PrepareKeyDir of an existing private directory: %v", err)
	}
	if enforcesModes {
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := credentials.PrepareKeyDir(dir); !errors.Is(err, credentials.ErrDirectoryPermissions) {
			t.Errorf("PrepareKeyDir of a writable directory: %v", err)
		}
	}
}

func TestKeyFilesAreDirectoriesOrOtherNonFilesAreRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A directory named like the key file is not a key.
	if err := os.Mkdir(filepath.Join(dir, "production.key"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := credentials.LoadKeys(dir, "production", noEnvironment); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a directory as the key file: %v", err)
	}
	// The same for the encrypted file.
	if err := os.Mkdir(filepath.Join(dir, "production.json.enc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.ReadFile(dir, "production"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a directory as the credentials file: %v", err)
	}
}
