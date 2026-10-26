package credentials_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rocketinsidecorp/forge-go/credentials"
	"github.com/rocketinsidecorp/forge-go/encryption"
)

const document = `{"stripe": {"api_key": "sk_live_SUPERSECRET", "retries": 3}, "empty": "", "flag": true}`

func newKey(t testing.TB, fill byte) encryption.Key {
	t.Helper()
	key, err := credentials.NewKey(bytes.Repeat([]byte{fill}, encryption.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func lookupWith(key encryption.Key) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if name == credentials.EnvMasterKey {
			return encryption.EncodeKey(key.Material), true
		}
		return "", false
	}
}

func writeCredentials(t testing.TB, dir, env, doc string, key encryption.Key) {
	t.Helper()
	sealed, err := credentials.Seal(env, []byte(doc), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteFile(dir, env, sealed, true); err != nil {
		t.Fatal(err)
	}
}

func TestSealOpenRoundTripAndEnvelopeFormat(t *testing.T) {
	t.Parallel()
	key := newKey(t, 1)
	sealed, err := credentials.Seal("production", []byte(document), key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("SUPERSECRET")) || bytes.Contains(sealed, []byte("stripe")) {
		t.Fatalf("plaintext in the file:\n%s", sealed)
	}
	var header map[string]any
	if err := json.Unmarshal(sealed, &header); err != nil {
		t.Fatal(err)
	}
	if header["version"] != float64(1) || header["algorithm"] != "AES-256-GCM" || header["key_id"] != key.ID || header["nonce"] == "" || header["ciphertext"] == "" || len(header) != 5 {
		t.Fatalf("unexpected envelope: %v", header)
	}
	plain, err := credentials.Unseal("production", sealed, []encryption.Key{key})
	if err != nil || string(plain) != document {
		t.Fatalf("Unseal = %q, %v", plain, err)
	}
	again, _ := credentials.Seal("production", []byte(document), key)
	if bytes.Equal(sealed, again) {
		t.Fatal("two seals of one document are identical: nonce reuse")
	}
	if id, err := credentials.KeyID(sealed); err != nil || id != key.ID {
		t.Fatalf("KeyID = %q, %v", id, err)
	}
}

func TestUnsealRejectsWrongKeyEnvironmentAndTampering(t *testing.T) {
	t.Parallel()
	key := newKey(t, 1)
	sealed, _ := credentials.Seal("production", []byte(document), key)

	// Same bytes presented as another environment's file: the staging-for-
	// production swap.
	if _, err := credentials.Unseal("staging", sealed, []encryption.Key{key}); !errors.Is(err, credentials.ErrDecrypt) {
		t.Fatalf("environment swap: %v", err)
	}
	// A different key that claims the same id.
	impostor := newKey(t, 2)
	impostor.ID = key.ID
	if _, err := credentials.Unseal("production", sealed, []encryption.Key{impostor}); !errors.Is(err, credentials.ErrDecrypt) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := credentials.Unseal("production", sealed, []encryption.Key{newKey(t, 3)}); !errors.Is(err, credentials.ErrNoMatchingKey) {
		t.Fatalf("no matching key: %v", err)
	}

	var header map[string]any
	_ = json.Unmarshal(sealed, &header)
	mutate := func(name string, change func(map[string]any)) {
		t.Helper()
		clone := map[string]any{}
		for k, v := range header {
			clone[k] = v
		}
		change(clone)
		data, _ := json.Marshal(clone)
		if _, err := credentials.Unseal("production", data, []encryption.Key{key}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	mutate("version downgrade", func(m map[string]any) { m["version"] = 0 })
	mutate("version bump", func(m map[string]any) { m["version"] = 2 })
	mutate("algorithm", func(m map[string]any) { m["algorithm"] = "none" })
	mutate("ciphertext flip", func(m map[string]any) {
		ct := []byte(m["ciphertext"].(string))
		ct[3] ^= 1
		m["ciphertext"] = string(ct)
	})
	mutate("nonce flip", func(m map[string]any) {
		n := []byte(m["nonce"].(string))
		n[0] = 'A' + (n[0]+1)%26
		m["nonce"] = string(n)
	})
	mutate("key id relabel", func(m map[string]any) {
		m["key_id"] = "0000000000000000"
	})
	mutate("extra field", func(m map[string]any) { m["extra"] = "x" })
	for _, junk := range []string{"", "{}", "not json", `{"version":1}`, string(sealed) + "{}"} {
		if _, err := credentials.Unseal("production", []byte(junk), []encryption.Key{key}); err == nil {
			t.Errorf("junk %q accepted", junk)
		}
	}
}

func TestSealValidatesDocument(t *testing.T) {
	t.Parallel()
	key := newKey(t, 1)
	for name, doc := range map[string]string{
		"array":          `[1]`,
		"scalar":         `"x"`,
		"null":           `null`,
		"syntax":         `{"a": }`,
		"unterminated":   `{"a": 1`,
		"duplicate":      `{"a": 1, "a": 2}`,
		"nested dup":     `{"a": {"b": 1, "b": 2}}`,
		"dup in array":   `{"a": [{"b": 1, "b": 2}]}`,
		"trailing":       `{"a": 1} {"b": 2}`,
		"trailing token": `{"a": 1} 5`,
		"empty":          ``,
	} {
		if _, err := credentials.Seal("production", []byte(doc), key); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := credentials.Seal("production", []byte("{}"), key); err != nil {
		t.Errorf("empty object rejected: %v", err)
	}
	for _, bad := range []string{"", "../production", "Prod", "a/b", "a.b", strings.Repeat("a", 33), "1x"} {
		if _, err := credentials.Seal(bad, []byte("{}"), key); !errors.Is(err, credentials.ErrInvalidEnvironment) {
			t.Errorf("environment %q: %v", bad, err)
		}
	}
}

func TestDocumentErrorsNeverEchoValues(t *testing.T) {
	t.Parallel()
	err := credentials.ParseDocument([]byte("{\n  \"token\": \"sk_live_SUPERSECRET\" \"x\": 1\n}"))
	if err == nil || strings.Contains(err.Error(), "SUPERSECRET") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error = %v", err)
	}
	err = credentials.ParseDocument([]byte(`{"sk_live_SUPERSECRET": 1, "sk_live_SUPERSECRET": 2}`))
	if err == nil || strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestOpenReadsTypedValues(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key := newKey(t, 1)
	writeCredentials(t, dir, "production", document, key)
	store, err := credentials.Open("production", credentials.Options{Dir: dir, Lookup: lookupWith(key)})
	if err != nil {
		t.Fatal(err)
	}
	if !store.Exists() || store.Environment() != "production" {
		t.Fatal("store metadata")
	}
	if got, err := store.String("stripe", "api_key"); err != nil || got != "sk_live_SUPERSECRET" {
		t.Fatalf("String = %q, %v", got, err)
	}
	secret, err := store.Secret("stripe", "api_key")
	if err != nil || secret.Reveal() != "sk_live_SUPERSECRET" || fmt.Sprint(secret) != "[REDACTED]" {
		t.Fatalf("Secret = %v, %v", secret, err)
	}
	if _, err := store.String("stripe", "missing"); !errors.Is(err, credentials.ErrMissing) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := store.String("stripe", "retries"); !errors.Is(err, credentials.ErrType) || strings.Contains(err.Error(), "3") {
		t.Fatalf("type: %v", err)
	}
	if _, err := store.String("stripe", "api_key", "deeper"); !errors.Is(err, credentials.ErrMissing) {
		t.Fatalf("descending into a string: %v", err)
	}
	if _, err := store.String(); !errors.Is(err, credentials.ErrMissing) {
		t.Fatalf("empty path: %v", err)
	}
	if !store.Has("flag") || store.Has("nope") {
		t.Fatal("Has")
	}
	var typed struct {
		APIKey  string `json:"api_key"`
		Retries int    `json:"retries"`
	}
	if err := store.Decode(&typed, "stripe"); err != nil || typed.APIKey != "sk_live_SUPERSECRET" || typed.Retries != 3 {
		t.Fatalf("Decode = %+v, %v", typed, err)
	}
	var whole struct {
		Flag bool `json:"flag"`
	}
	if err := store.Decode(&whole); err != nil || !whole.Flag {
		t.Fatalf("Decode whole = %+v, %v", whole, err)
	}
	var wrong struct {
		APIKey int `json:"api_key"`
	}
	if err := store.Decode(&wrong, "stripe"); err == nil || strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatalf("Decode type error: %v", err)
	}
}

func TestKeyFromFileAndPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	t.Parallel()
	dir := t.TempDir()
	key := newKey(t, 4)
	writeCredentials(t, dir, "production", document, key)
	noEnv := func(string) (string, bool) { return "", false }

	if _, err := credentials.Open("production", credentials.Options{Dir: dir, Lookup: noEnv}); !errors.Is(err, credentials.ErrKeyMissing) {
		t.Fatalf("no key anywhere: %v", err)
	}
	if err := credentials.WriteKeyFile(dir, "production", []encryption.Key{key}, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "production.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	if _, err := credentials.Open("production", credentials.Options{Dir: dir, Lookup: noEnv}); err != nil {
		t.Fatalf("key file: %v", err)
	}
	if err := credentials.WriteKeyFile(dir, "production", []encryption.Key{key}, false); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("a key file must not be overwritten by accident: %v", err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660} {
		if err := os.Chmod(filepath.Join(dir, "production.key"), mode); err != nil {
			t.Fatal(err)
		}
		if _, err := credentials.Open("production", credentials.Options{Dir: dir, Lookup: noEnv}); !errors.Is(err, credentials.ErrKeyPermissions) {
			t.Errorf("mode %04o accepted: %v", mode, err)
		}
	}
	_ = os.Chmod(filepath.Join(dir, "production.key"), 0o400)
	if _, err := credentials.Open("production", credentials.Options{Dir: dir, Lookup: noEnv}); err != nil {
		t.Errorf("read-only owner key rejected: %v", err)
	}
	// The environment variable wins over the file.
	if _, err := credentials.Open("production", credentials.Options{Dir: dir, Lookup: lookupWith(newKey(t, 9))}); !errors.Is(err, credentials.ErrNoMatchingKey) {
		t.Fatalf("env key should override the file: %v", err)
	}
	// Errors from key parsing never echo the value.
	_, err = credentials.Open("production", credentials.Options{Dir: dir, Lookup: func(string) (string, bool) { return "sekret-not-a-key", true }})
	if err == nil || strings.Contains(err.Error(), "sekret") {
		t.Fatalf("bad FORGE_MASTER_KEY error: %v", err)
	}
}

func TestSymlinkedKeyFileIsNotFollowedOutsideTheDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	t.Parallel()
	dir, outside := t.TempDir(), t.TempDir()
	key := newKey(t, 5)
	writeCredentials(t, dir, "production", document, key)
	if err := credentials.WriteKeyFile(outside, "production", []encryption.Key{key}, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "production.key"), filepath.Join(dir, "production.key")); err != nil {
		t.Fatal(err)
	}
	noEnv := func(string) (string, bool) { return "", false }
	if _, err := credentials.Open("production", credentials.Options{Dir: dir, Lookup: noEnv}); !errors.Is(err, credentials.ErrSymlink) {
		t.Fatalf("a key file symlinked outside the credentials directory: %v", err)
	}
}

func TestMissingFileAndRequiredCredentials(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key := newKey(t, 1)
	opts := credentials.Options{Dir: dir, Lookup: lookupWith(key)}

	dev, err := credentials.Open("development", opts)
	if err != nil || dev.Exists() {
		t.Fatalf("development without a file: %v exists=%v", err, dev != nil && dev.Exists())
	}
	if _, err := dev.String("a"); !errors.Is(err, credentials.ErrMissing) {
		t.Fatalf("empty dev store: %v", err)
	}
	for _, env := range []string{"production", "staging"} {
		if _, err := credentials.Open(env, opts); !errors.Is(err, credentials.ErrNotFound) {
			t.Fatalf("%s without a file must fail fast: %v", env, err)
		}
	}

	writeCredentials(t, dir, "production", document, key)
	opts.Required = []string{"stripe.api_key", "database.url", "empty", "stripe.retries.deeper"}
	_, err = credentials.Open("production", opts)
	if !errors.Is(err, credentials.ErrMissing) {
		t.Fatalf("missing required credentials: %v", err)
	}
	for _, want := range []string{"database.url", "empty", "stripe.retries.deeper"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "stripe.api_key") || strings.Contains(err.Error(), "SUPERSECRET") {
		t.Errorf("error names a present credential or a value: %v", err)
	}
	opts.Required = []string{"stripe.api_key", "flag"}
	if _, err := credentials.Open("production", opts); err != nil {
		t.Fatalf("all required present: %v", err)
	}
	// Development tolerates missing required credentials, but an unreadable
	// file still fails closed.
	writeCredentials(t, dir, "development", `{}`, key)
	opts.Required = []string{"absent"}
	if _, err := credentials.Open("development", opts); err != nil {
		t.Fatalf("development tolerates absent credentials: %v", err)
	}
	opts.Lookup = lookupWith(newKey(t, 8))
	if _, err := credentials.Open("development", opts); err == nil {
		t.Fatal("an undecryptable development file must fail closed")
	}
}

func TestEnvironmentSwapOnDiskIsRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key := newKey(t, 1) // the same key everywhere makes the swap the only defense
	writeCredentials(t, dir, "staging", `{"db":"staging-db"}`, key)
	writeCredentials(t, dir, "production", `{"db":"prod-db"}`, key)
	staging, err := os.ReadFile(filepath.Join(dir, "staging.json.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "production.json.enc"), staging, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = credentials.Open("production", credentials.Options{Dir: dir, Lookup: lookupWith(key)})
	if !errors.Is(err, credentials.ErrDecrypt) {
		t.Fatalf("staging file installed as production: %v", err)
	}
}

func TestKeyRotationWithOldKeyKept(t *testing.T) {
	t.Parallel()
	oldKey, newKeyV := newKey(t, 1), newKey(t, 2)
	oldFile, _ := credentials.Seal("production", []byte(document), oldKey)
	ring := []encryption.Key{newKeyV, oldKey}
	if plain, err := credentials.Unseal("production", oldFile, ring); err != nil || string(plain) != document {
		t.Fatalf("old file with ring: %v", err)
	}
	if _, err := credentials.Unseal("production", oldFile, []encryption.Key{newKeyV}); !errors.Is(err, credentials.ErrNoMatchingKey) {
		t.Fatalf("old file without the old key: %v", err)
	}
	keys, err := credentials.ParseKeys(encryption.EncodeKey(newKeyV.Material) + ",\n# previous\n" + encryption.EncodeKey(oldKey.Material) + "\n")
	if err != nil || len(keys) != 2 || keys[0].ID != newKeyV.ID || keys[1].ID != oldKey.ID {
		t.Fatalf("ParseKeys: %v %v", keys, err)
	}
	if _, err := credentials.ParseKeys(" \n# nothing\n"); err == nil {
		t.Error("no keys accepted")
	}
}

func TestEmbeddedFilesystem(t *testing.T) {
	t.Parallel()
	key := newKey(t, 1)
	sealed, _ := credentials.Seal("production", []byte(document), key)
	fsys := fstest.MapFS{"config/credentials/production.json.enc": {Data: sealed}}
	store, err := credentials.Open("production", credentials.Options{FS: fsys, Lookup: lookupWith(key)})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := store.String("stripe", "api_key"); got != "sk_live_SUPERSECRET" {
		t.Fatalf("embedded read = %q", got)
	}
	noEnv := func(string) (string, bool) { return "", false }
	if _, err := credentials.Open("production", credentials.Options{FS: fsys, Lookup: noEnv}); !errors.Is(err, credentials.ErrKeyMissing) {
		t.Fatalf("embedded file without FORGE_MASTER_KEY: %v", err)
	}
	if _, err := credentials.Open("production", credentials.Options{FS: fstest.MapFS{}, Lookup: lookupWith(key)}); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("embedded file missing: %v", err)
	}
}

func TestStoreIsRedactedEverywhere(t *testing.T) {
	t.Parallel()
	store, err := credentials.Parse("production", []byte(document))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("store", "credentials", store, "value", *store)
	encoded, _ := json.Marshal(struct{ C *credentials.Store }{store})
	for name, out := range map[string]string{
		"%v": fmt.Sprintf("%v", store), "%+v": fmt.Sprintf("%+v", store), "%#v": fmt.Sprintf("%#v", store),
		"%v value": fmt.Sprintf("%v", *store), "%s": fmt.Sprintf("%s", store), "slog": logs.String(), "json": string(encoded),
	} {
		if strings.Contains(out, "SUPERSECRET") || strings.Contains(out, "stripe") {
			t.Errorf("%s leaked: %s", name, out)
		}
	}
}

func TestKeyRingFromCredentials(t *testing.T) {
	t.Parallel()
	a, b := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	doc := fmt.Sprintf(`{"encryption": {"active": "b", "keys": {"a": %q, "b": %q}}}`, encryption.EncodeKey(a), encryption.EncodeKey(b))
	store, err := credentials.Parse("production", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := store.KeyRing("encryption")
	if err != nil || ring.ActiveID() != "b" {
		t.Fatalf("KeyRing: %v %v", ring, err)
	}
	if _, err := encryption.NewCipher(ring); err != nil {
		t.Fatal(err)
	}
	if _, err := store.KeyRing("missing"); !errors.Is(err, credentials.ErrMissing) {
		t.Fatalf("missing key ring: %v", err)
	}
}

func TestWriteFileIsAtomicAndKeepsNoTemporaries(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "config", "credentials")
	key := newKey(t, 1)
	sealed, _ := credentials.Seal("production", []byte(document), key)
	if err := credentials.WriteFile(dir, "production", sealed, false); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteFile(dir, "production", sealed, false); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("create-only overwrite: %v", err)
	}
	if err := credentials.WriteFile(dir, "production", sealed, true); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "production.json.enc" {
		t.Fatalf("directory holds %v; want only the credentials file", entries)
	}
}

type storeHolder struct {
	creds credentials.Store
	Open  credentials.Store
}

type embeddedStore struct{ credentials.Store }

// TestStoreIsRedactedInUnexportedAndEmbeddedFields: fmt cannot call Format or
// GoString on an unexported field, so a Store embedded by value must not hold
// its content inline.
func TestStoreIsRedactedInUnexportedAndEmbeddedFields(t *testing.T) {
	t.Parallel()
	store, err := credentials.Parse("production", []byte(document))
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{
		"unexported value": storeHolder{creds: *store, Open: *store},
		"pointer holder":   &storeHolder{creds: *store},
		"embedded":         embeddedStore{*store},
		"slice":            []credentials.Store{*store},
		"map":              map[string]credentials.Store{"k": *store},
	} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			out := fmt.Sprintf(verb, v)
			if strings.Contains(out, "SUPERSECRET") || strings.Contains(out, "stripe") {
				t.Errorf("%s with %s leaked: %s", name, verb, out)
			}
		}
	}
	var zero credentials.Store // the zero value is usable and empty
	if zero.Exists() || zero.Has("a") || zero.Environment() != "" {
		t.Error("zero Store is not empty")
	}
	if _, err := zero.String("a"); !errors.Is(err, credentials.ErrMissing) {
		t.Errorf("zero Store lookup: %v", err)
	}
}

func TestWriteFileCreatesTheDirectoryPrivately(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "config", "credentials")
	sealed, _ := credentials.Seal("production", []byte("{}"), newKey(t, 1))
	if err := credentials.WriteFile(dir, "production", sealed, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("credentials directory mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
}
