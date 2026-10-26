package credentials_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rocketinsidecorp/forge-go/credentials"
	"github.com/rocketinsidecorp/forge-go/encryption"
)

// enforcesModes reports whether file modes are enforced where the tests run.
var enforcesModes = runtime.GOOS != "windows"

func noEnvironment(string) (string, bool) { return "", false }

// Load reads config/credentials relative to the working directory and the key
// from the process environment or the key file: the path `forge credentials
// init` and the generated application use.
func TestLoadAndInitUseTheDefaultDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(credentials.EnvMasterKey, "")

	// Development and test tolerate a missing file; production does not.
	for _, env := range []string{"development", "test"} {
		store, err := credentials.Load(env)
		if err != nil || store.Exists() || store.Environment() != env || store.Has("anything") {
			t.Errorf("Load(%s) without a file = %v, %v", env, store, err)
		}
	}
	_, err := credentials.Load("production")
	if !errors.Is(err, credentials.ErrNotFound) || !strings.Contains(err.Error(), "config/credentials/production.json.enc") {
		t.Fatalf("Load(production) without a file: %v", err)
	}
	if _, err := credentials.Load("../production"); !errors.Is(err, credentials.ErrInvalidEnvironment) {
		t.Fatalf("Load with a path as the environment: %v", err)
	}

	created, err := credentials.Init("", "production")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(credentials.DefaultDir, "production.key"),
		filepath.Join(credentials.DefaultDir, "production.json.enc"),
	}
	if fmt.Sprint(created) != fmt.Sprint(want) {
		t.Fatalf("Init created %v, want %v", created, want)
	}
	store, err := credentials.Load("production")
	if err != nil || !store.Exists() || store.Environment() != "production" {
		t.Fatalf("Load after Init = %v, %v", store, err)
	}
	// The skeleton holds a usable key ring and nothing else.
	ring, err := store.KeyRing("encryption")
	if err != nil || ring == nil {
		t.Fatalf("KeyRing of the skeleton = %v, %v", ring, err)
	}
	// Another key in the environment does not open the file.
	t.Setenv(credentials.EnvMasterKey, encryption.EncodeKey(bytes.Repeat([]byte{7}, encryption.KeySize)))
	if _, err := credentials.Load("production"); !errors.Is(err, credentials.ErrNoMatchingKey) && !errors.Is(err, credentials.ErrDecrypt) {
		t.Fatalf("Load with another master key: %v", err)
	}
}

func TestInitCreatesPrivateFilesAndNeverReplacesAnything(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "credentials")
	if _, err := credentials.Init(dir, "Production"); !errors.Is(err, credentials.ErrInvalidEnvironment) {
		t.Fatalf("Init with an invalid environment: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an invalid environment created the directory: %v", err)
	}
	created, err := credentials.Init(dir, "staging")
	if err != nil || len(created) != 2 {
		t.Fatalf("Init = %v, %v", created, err)
	}
	keyPath, filePath := created[0], created[1]
	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, %v", info, err)
	}
	keyBefore, _ := os.ReadFile(keyPath)
	fileBefore, _ := os.ReadFile(filePath)

	// A second Init reuses the key, refuses to replace the file and creates
	// nothing: neither file changes.
	again, err := credentials.Init(dir, "staging")
	if err == nil || !strings.Contains(err.Error(), "already exists") || len(again) != 0 {
		t.Fatalf("second Init = %v, %v", again, err)
	}
	keyAfter, _ := os.ReadFile(keyPath)
	fileAfter, _ := os.ReadFile(filePath)
	if !bytes.Equal(keyBefore, keyAfter) || !bytes.Equal(fileBefore, fileAfter) {
		t.Fatal("a refused Init changed a file")
	}

	// With the file gone and the key restored, Init creates only the file,
	// and the restored key opens it.
	if err := os.Remove(filePath); err != nil {
		t.Fatal(err)
	}
	again, err = credentials.Init(dir, "staging")
	if err != nil || len(again) != 1 || again[0] != filePath {
		t.Fatalf("Init with a restored key = %v, %v", again, err)
	}
	if keyNow, _ := os.ReadFile(keyPath); !bytes.Equal(keyBefore, keyNow) {
		t.Fatal("Init rewrote the existing key")
	}
	if _, err := credentials.Open("staging", credentials.Options{Dir: dir, Lookup: noEnvironment}); err != nil {
		t.Fatalf("the restored key does not open the new file: %v", err)
	}
}

func TestInitRefusesAnUnusableDirectoryOrKey(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	// A key file others can read is refused, and nothing is created beside it.
	loose := filepath.Join(base, "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	key := newKey(t, 4)
	if err := credentials.WriteKeyFile(loose, "production", []encryption.Key{key}, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(loose, "production.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	created, err := credentials.Init(loose, "production")
	if enforcesModes && (!errors.Is(err, credentials.ErrKeyPermissions) || len(created) != 0) {
		t.Errorf("Init with a world-readable key = %v, %v", created, err)
	}
	if _, statErr := os.Stat(filepath.Join(loose, "production.json.enc")); enforcesModes && statErr == nil {
		t.Error("Init wrote a credentials file next to a refused key")
	}

	// A directory that is really a file cannot hold credentials.
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err := credentials.Init(filepath.Join(file, "credentials"), "production"); err == nil || len(created) != 0 {
		t.Errorf("Init below a regular file = %v, %v", created, err)
	}
}

func TestLoadKeysRefusesWhatIsNotAKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := credentials.LoadKeys(dir, "Bad Env", noEnvironment); !errors.Is(err, credentials.ErrInvalidEnvironment) {
		t.Errorf("an invalid environment: %v", err)
	}
	write("huge.key", strings.Repeat("A", 4097))
	if _, _, err := credentials.LoadKeys(dir, "huge", noEnvironment); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("an oversized key file: %v", err)
	}
	write("garbage.key", "hunter2-not-a-key\n")
	if _, _, err := credentials.LoadKeys(dir, "garbage", noEnvironment); err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "garbage.key") {
		t.Errorf("a key file that is not a key: %v", err)
	}
	var many strings.Builder
	for i := range credentials.MaxKeys + 1 {
		many.WriteString(encryption.EncodeKey(bytes.Repeat([]byte{byte(i + 1)}, encryption.KeySize)) + "\n")
	}
	write("many.key", many.String())
	if _, _, err := credentials.LoadKeys(dir, "many", noEnvironment); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("too many keys: %v", err)
	}
	// FORGE_MASTER_KEY that is not a key names the variable, never its value.
	bad := func(name string) (string, bool) { return "hunter2-not-a-key", name == credentials.EnvMasterKey }
	if _, _, err := credentials.LoadKeys(dir, "garbage", bad); err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), credentials.EnvMasterKey) {
		t.Errorf("an invalid FORGE_MASTER_KEY: %v", err)
	}
	// A directory that is a file is an error, not a missing key.
	file := filepath.Join(dir, "notadir")
	write("notadir", "x")
	if _, _, err := credentials.LoadKeys(file, "production", noEnvironment); err == nil || errors.Is(err, credentials.ErrKeyMissing) {
		t.Errorf("a regular file as the directory: %v", err)
	}
	if _, _, err := credentials.LoadKeys(filepath.Join(dir, "absent"), "production", noEnvironment); !errors.Is(err, credentials.ErrKeyMissing) {
		t.Errorf("a missing directory: %v", err)
	}
}

func TestKeySourceSaysWhereTheKeyCame(t *testing.T) {
	dir := t.TempDir()
	key := newKey(t, 5)
	if err := credentials.WriteKeyFile(dir, "production", []encryption.Key{key}, false); err != nil {
		t.Fatal(err)
	}
	// Without a lookup the process environment is read.
	t.Setenv(credentials.EnvMasterKey, encryption.EncodeKey(key.Material))
	keys, source, err := credentials.LoadKeys(dir, "production", nil)
	if err != nil || len(keys) != 1 || !source.Environment || source.String() != credentials.EnvMasterKey {
		t.Fatalf("from the environment: %v %+v %v", keys, source, err)
	}
	t.Setenv(credentials.EnvMasterKey, "")
	keys, source, err = credentials.LoadKeys(dir, "production", nil)
	want := filepath.Join(dir, "production.key")
	if err != nil || len(keys) != 1 || source.Environment || source.String() != want {
		t.Fatalf("from the file: %v %+v %v", keys, source, err)
	}
	// The embedded filesystem path reads the process environment the same way.
	sealed, err := credentials.Seal("production", []byte(document), key)
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"config/credentials/production.json.enc": {Data: sealed}}
	if _, err := credentials.Open("production", credentials.Options{FS: fsys}); !errors.Is(err, credentials.ErrKeyMissing) {
		t.Errorf("embedded file, empty FORGE_MASTER_KEY: %v", err)
	}
	t.Setenv(credentials.EnvMasterKey, encryption.EncodeKey(key.Material))
	if store, err := credentials.Open("production", credentials.Options{FS: fsys}); err != nil || !store.Has("stripe") {
		t.Errorf("embedded file, FORGE_MASTER_KEY set: %v", err)
	}
	t.Setenv(credentials.EnvMasterKey, "hunter2-not-a-key")
	if _, err := credentials.Open("production", credentials.Options{FS: fsys}); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("embedded file, invalid FORGE_MASTER_KEY: %v", err)
	}
}

// brokenFS fails every read with err.
type brokenFS struct{ err error }

func (b brokenFS) Open(string) (fs.File, error) { return nil, b.err }

func TestOpenRefusesFilesItCannotTrust(t *testing.T) {
	t.Parallel()
	key := newKey(t, 1)
	opts := func(fsys fs.FS) credentials.Options {
		return credentials.Options{FS: fsys, Lookup: lookupWith(key)}
	}
	if _, err := credentials.Open("Production", credentials.Options{}); !errors.Is(err, credentials.ErrInvalidEnvironment) {
		t.Errorf("Open with an invalid environment: %v", err)
	}
	// A read error other than "not found" is reported as it is, in every
	// environment: development must not quietly run without its secrets.
	denied := brokenFS{err: fs.ErrPermission}
	for _, env := range []string{"development", "production"} {
		if _, err := credentials.Open(env, opts(denied)); !errors.Is(err, fs.ErrPermission) || errors.Is(err, credentials.ErrNotFound) {
			t.Errorf("%s with an unreadable embedded file: %v", env, err)
		}
	}
	huge := fstest.MapFS{"config/credentials/production.json.enc": {Data: bytes.Repeat([]byte{'x'}, credentials.MaxFileBytes+1)}}
	if _, err := credentials.Open("production", opts(huge)); !errors.Is(err, credentials.ErrFormat) {
		t.Errorf("an oversized embedded file: %v", err)
	}
	// A file that is not a credentials file is a format error.
	junk := fstest.MapFS{"config/credentials/production.json.enc": {Data: []byte("not json")}}
	if _, err := credentials.Open("production", opts(junk)); !errors.Is(err, credentials.ErrFormat) {
		t.Errorf("a junk embedded file: %v", err)
	}
}

func TestParseRefusesInvalidInput(t *testing.T) {
	t.Parallel()
	if _, err := credentials.Parse("Prod", []byte(`{}`)); !errors.Is(err, credentials.ErrInvalidEnvironment) {
		t.Errorf("Parse with an invalid environment: %v", err)
	}
	if _, err := credentials.Parse("production", []byte(`{"a": 1`)); err == nil {
		t.Error("Parse accepted a truncated document")
	}
	store, err := credentials.Parse("production", []byte(document))
	if err != nil || !store.Exists() || store.Environment() != "production" || !store.Has("stripe", "api_key") {
		t.Errorf("Parse = %v, %v", store, err)
	}
}

func TestDocumentsAreValidatedStrictly(t *testing.T) {
	t.Parallel()
	tooBig := `{"k": "` + strings.Repeat("a", credentials.MaxPlaintextBytes) + `"}`
	for name, doc := range map[string]string{
		"too large":              tooBig,
		"trailing garbage":       `{"a": 1} hunter2`,
		"a second object":        `{"a": 1} {"b": 2}`,
		"truncated in an array":  `{"a": [1, 2`,
		"truncated in an object": `{"a": {"b": "hunter2"`,
		"an array":               `["hunter2"]`,
		"null":                   `null`,
		"duplicate key":          "{\n  \"a\": 1,\n  \"a\": \"hunter2\"\n}",
		"a nested duplicate":     `{"a": {"b": 1, "b": 2}}`,
		"a duplicate in a list":  `{"a": [{"b": 1, "b": 2}]}`,
		"a syntax error":         `{"a": hunter2}`,
		"empty":                  ``,
	} {
		err := credentials.ParseDocument([]byte(doc))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error echoes a value: %v", name, err)
		}
	}
	err := credentials.ParseDocument([]byte("{\n  \"a\": 1,\n  \"a\": 2\n}"))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("a duplicate key is reported with its line: %v", err)
	}
	if err := credentials.ParseDocument([]byte(`{"a": [1, {"b": [true, null]}], "c": {}}`)); err != nil {
		t.Errorf("a valid nested document: %v", err)
	}
}

func TestEnvelopesAreValidatedBeforeDecryption(t *testing.T) {
	t.Parallel()
	key := newKey(t, 1)
	sealed, err := credentials.Seal("production", []byte(document), key)
	if err != nil {
		t.Fatal(err)
	}
	edit := func(field, value string) []byte {
		var fields map[string]any
		if err := json.Unmarshal(sealed, &fields); err != nil {
			t.Fatal(err)
		}
		fields[field] = value
		out, _ := json.Marshal(fields)
		return out
	}
	keys := []encryption.Key{key}
	for name, file := range map[string][]byte{
		"a nonce of the wrong length": edit("nonce", "AAAA"),
		"a nonce that is not base64":  edit("nonce", "!!!!"),
		"ciphertext not base64":       edit("ciphertext", "!!!!"),
		"an oversized file":           bytes.Repeat([]byte{' '}, credentials.MaxFileBytes+1),
	} {
		if _, err := credentials.Unseal("production", file, keys); !errors.Is(err, credentials.ErrFormat) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := credentials.KeyID(file); name == "an oversized file" && !errors.Is(err, credentials.ErrFormat) {
			t.Errorf("KeyID of %s: %v", name, err)
		}
	}
	if id, err := credentials.KeyID(sealed); err != nil || id != key.ID {
		t.Errorf("KeyID = %q, %v", id, err)
	}
	if _, err := credentials.KeyID([]byte("{}")); !errors.Is(err, credentials.ErrFormat) {
		t.Errorf("KeyID of an empty envelope: %v", err)
	}
	if _, err := credentials.Unseal("Production", sealed, keys); !errors.Is(err, credentials.ErrInvalidEnvironment) {
		t.Errorf("Unseal with an invalid environment: %v", err)
	}
}

func TestKeysOfTheWrongSizeAreRefused(t *testing.T) {
	t.Parallel()
	short := encryption.Key{ID: "short", Material: []byte("too short")}
	if _, err := credentials.NewKey(short.Material); !errors.Is(err, encryption.ErrInvalidKey) {
		t.Errorf("NewKey of a short key: %v", err)
	}
	if _, err := credentials.Seal("production", []byte(document), short); !errors.Is(err, encryption.ErrInvalidKey) {
		t.Errorf("Seal with a short key: %v", err)
	}
	good := newKey(t, 1)
	sealed, err := credentials.Seal("production", []byte(document), good)
	if err != nil {
		t.Fatal(err)
	}
	// The same key id with different, short material cannot open the file.
	impostor := encryption.Key{ID: good.ID, Material: []byte("too short")}
	if _, err := credentials.Unseal("production", sealed, []encryption.Key{impostor}); !errors.Is(err, encryption.ErrInvalidKey) {
		t.Errorf("Unseal with a short key: %v", err)
	}
	if _, err := credentials.Seal("production", []byte(`{"a": 1, "a": 2}`), good); err == nil {
		t.Error("Seal accepted a document with a duplicate key")
	}
}

func TestStoreReportsTypesAndEmptyValuesWithoutEchoingThem(t *testing.T) {
	t.Parallel()
	store, err := credentials.Parse("production", []byte(`{
		"text": "hunter2", "empty": "", "nothing": null, "zero": 0, "no": false,
		"list": [1], "emptyList": [], "object": {"k": "v"}, "emptyObject": {}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"nothing": "null", "zero": "a number", "no": "a boolean", "list": "an array", "object": "an object",
	} {
		if _, err := store.String(path); !errors.Is(err, credentials.ErrType) || !strings.Contains(err.Error(), want) {
			t.Errorf("String(%s) = %v, want a type error saying %q", path, err, want)
		}
	}
	if got, err := store.String("empty"); err != nil || got != "" {
		t.Errorf("String of an empty string = %q, %v", got, err)
	}
	if _, err := store.Secret("zero"); !errors.Is(err, credentials.ErrType) {
		t.Errorf("Secret of a number: %v", err)
	}
	if _, err := store.Secret("absent"); !errors.Is(err, credentials.ErrMissing) {
		t.Errorf("Secret of a missing path: %v", err)
	}
	// Require: absent, null, "", [] and {} are missing; 0, false and filled
	// containers are present. Everything missing is named at once, sorted.
	err = store.Require("text", "zero", "no", "list", "object", "nothing", "empty", "emptyList", "emptyObject", "absent")
	if !errors.Is(err, credentials.ErrMissing) {
		t.Fatalf("Require: %v", err)
	}
	if want := "absent, empty, emptyList, emptyObject, nothing"; !strings.Contains(err.Error(), want) {
		t.Errorf("Require names %q, want %q", err, want)
	}
	for _, present := range []string{"text", "zero", "no", "list", "object"} {
		if strings.Contains(err.Error(), present+",") || strings.HasSuffix(err.Error(), present) {
			t.Errorf("Require reports the present path %s: %v", present, err)
		}
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("Require echoes a value: %v", err)
	}
	if err := store.Require(); err != nil {
		t.Errorf("Require of nothing: %v", err)
	}
}

func TestDecodeAndKeyRingReportBadTargetsWithoutValues(t *testing.T) {
	t.Parallel()
	store, err := credentials.Parse("production", []byte(`{"text": "hunter2", "ring": {"active": "k1", "keys": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var notAPointer struct{ Text string }
	if err := store.Decode(notAPointer); err == nil || !strings.Contains(err.Error(), "invalid structure") {
		t.Errorf("Decode into a non-pointer: %v", err)
	}
	var wrong struct {
		Text []string `json:"text"`
	}
	if err := store.Decode(&wrong); err == nil || !strings.Contains(err.Error(), "wrong type") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("Decode into the wrong type: %v", err)
	}
	if err := store.Decode(&wrong, "absent"); !errors.Is(err, credentials.ErrMissing) {
		t.Errorf("Decode of a missing path: %v", err)
	}
	if _, err := store.KeyRing("absent"); !errors.Is(err, credentials.ErrMissing) {
		t.Errorf("KeyRing of a missing path: %v", err)
	}
	if _, err := store.KeyRing("ring"); err == nil {
		t.Error("KeyRing accepted a ring whose active key is not in it")
	}
}

func TestStoreFormatsRedactedInEveryVerb(t *testing.T) {
	t.Parallel()
	store, err := credentials.Parse("production", []byte(document))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{fmt.Sprintf("%#v", *store), fmt.Sprintf("%v", *store), fmt.Sprintf("%s", store), fmt.Sprintf("%d", *store)} {
		if text != "credentials.Store(production, [REDACTED])" {
			t.Errorf("formatted Store = %q", text)
		}
	}
}
