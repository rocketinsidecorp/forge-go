// Package credentials reads Forge's encrypted per-environment credentials,
// the equivalent of Rails' credentials:edit. Secrets live in
// config/credentials/<env>.json.enc, safe to commit, encrypted with
// AES-256-GCM under a key that is never committed: FORGE_MASTER_KEY or the
// gitignored config/credentials/<env>.key. The file's format version, key id
// and environment name are authenticated, so a file from another environment
// cannot be substituted.
//
//	creds, err := credentials.Open(string(cfg.Environment), credentials.Options{
//		Required: []string{"stripe.api_key"}, // production fails fast if absent
//	})
//	if err != nil { ... }
//	stripeKey, err := creds.Secret("stripe", "api_key") // config.Secret, redacted when printed
//
// The package uses only the standard library and package encryption.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/encryption"
)

var (
	// ErrMissing reports a credential that is not present.
	ErrMissing = errors.New("credentials: missing credential")
	// ErrType reports a credential of an unexpected JSON type.
	ErrType = errors.New("credentials: unexpected credential type")
)

// Options tunes Open. The zero value reads config/credentials relative to the
// working directory and the key from the process environment.
type Options struct {
	// Dir is the credentials directory; DefaultDir when empty.
	Dir string
	// FS, when set, supplies the encrypted file instead of the disk, for
	// applications that embed it with //go:embed. Paths are Dir/<env>.json.enc
	// inside FS. The key then must come from FORGE_MASTER_KEY.
	FS fs.FS
	// Lookup reads FORGE_MASTER_KEY; os.LookupEnv when nil.
	Lookup func(string) (string, bool)
	// Required lists dotted paths (for example "stripe.api_key") that must
	// exist as non-empty values. They are enforced outside development and
	// test, so a production process fails at startup instead of on first use.
	Required []string
}

// Store is a decrypted credentials document. It is immutable and safe for
// concurrent use. Printing or logging a Store never shows its content.
//
// Store holds its content behind a function value. fmt cannot call Format or
// GoString on an unexported struct field and falls back to printing what it
// finds there, expanding structs, maps and even pointers to them (a bad verb
// such as %s does so too); a func value always prints as an address, so a
// Store embedded as an unexported field cannot reveal its secrets. As a
// consequence a Store is not comparable with ==.
type Store struct{ get func() *storeData }

type storeData struct {
	env    string
	data   map[string]any
	exists bool
}

var emptyStore = &storeData{data: map[string]any{}}

func newStore(d *storeData) *Store { return &Store{get: func() *storeData { return d }} }

func (s Store) content() *storeData {
	if s.get == nil {
		return emptyStore
	}
	return s.get()
}

// Load opens the credentials of env with default options.
func Load(env string) (*Store, error) { return Open(env, Options{}) }

// strict reports whether missing credentials are an error: every environment
// except development and test.
func strict(env string) bool { return env != "development" && env != "test" }

// Open reads and decrypts the credentials of env.
//
// In development and test a missing file yields an empty Store (Exists
// reports false). In every other environment, such as production, a missing
// file, a missing key or any missing Required credential is an error. In all
// environments an existing file that cannot be decrypted is an error:
// credentials fail closed.
func Open(env string, opts Options) (*Store, error) {
	if err := ValidateEnvironment(env); err != nil {
		return nil, err
	}
	dir := opts.Dir
	if dir == "" {
		dir = DefaultDir
	}
	file, err := readEncrypted(dir, env, opts.FS)
	if errors.Is(err, ErrNotFound) {
		if strict(env) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path.Join(filepathSlash(dir), FileName(env)))
		}
		return newStore(&storeData{env: env, data: map[string]any{}}), nil
	}
	if err != nil {
		return nil, err
	}
	var keys []encryption.Key
	if opts.FS != nil {
		keys, err = keysFromEnvironment(opts.Lookup)
	} else {
		keys, _, err = LoadKeys(dir, env, opts.Lookup)
	}
	if err != nil {
		return nil, err
	}
	plaintext, err := Unseal(env, file, keys)
	if err != nil {
		return nil, err
	}
	defer clear(plaintext)
	store, err := parseStore(env, plaintext)
	if err != nil {
		return nil, err
	}
	if strict(env) {
		if err := store.Require(opts.Required...); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func filepathSlash(dir string) string { return strings.ReplaceAll(dir, "\\", "/") }

func readEncrypted(dir, env string, fsys fs.FS) ([]byte, error) {
	if fsys == nil {
		return ReadFile(dir, env)
	}
	data, err := fs.ReadFile(fsys, path.Join(filepathSlash(dir), FileName(env)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("%w: file is too large", ErrFormat)
	}
	return data, nil
}

func keysFromEnvironment(lookup func(string) (string, bool)) ([]encryption.Key, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	value, ok := lookup(EnvMasterKey)
	if !ok || strings.TrimSpace(value) == "" {
		return nil, ErrKeyMissing
	}
	keys, err := ParseKeys(value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvMasterKey, err)
	}
	return keys, nil
}

// Parse builds a Store from a decrypted document, for tools such as the
// forge CLI that already hold the plaintext.
func Parse(env string, plaintext []byte) (*Store, error) {
	if err := ValidateEnvironment(env); err != nil {
		return nil, err
	}
	return parseStore(env, plaintext)
}

func parseStore(env string, plaintext []byte) (*Store, error) {
	doc, err := parseDocument(plaintext)
	if err != nil {
		return nil, err
	}
	return newStore(&storeData{env: env, data: doc, exists: true}), nil
}

// Environment returns the environment the store belongs to.
func (s Store) Environment() string { return s.content().env }

// Exists reports whether a credentials file was read. It is false only for a
// development or test environment without a file.
func (s Store) Exists() bool { return s.content().exists }

func (s Store) lookup(keys []string) (any, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: empty path", ErrMissing)
	}
	var current any = s.content().data
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrMissing, strings.Join(keys, "."))
		}
		if current, ok = object[key]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrMissing, strings.Join(keys, "."))
		}
	}
	return current, nil
}

// Has reports whether path exists.
func (s Store) Has(keys ...string) bool {
	_, err := s.lookup(keys)
	return err == nil
}

// String returns the string at path. A missing path is ErrMissing, a value of
// another JSON type is ErrType; errors name the path, never the value.
func (s Store) String(keys ...string) (string, error) {
	value, err := s.lookup(keys)
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: %q is %s, want a string", ErrType, strings.Join(keys, "."), jsonType(value))
	}
	return text, nil
}

// Secret returns the string at path as a config.Secret, which prints as
// [REDACTED] everywhere; call Reveal at the point of use.
func (s Store) Secret(keys ...string) (config.Secret, error) {
	text, err := s.String(keys...)
	if err != nil {
		return config.Secret{}, err
	}
	return config.NewSecret(text), nil
}

// Decode unmarshals the object at path (the whole document when no path is
// given) into v, which is typically a pointer to a struct with json tags.
// Unknown fields are ignored.
func (s Store) Decode(v any, keys ...string) error {
	var value any = s.content().data
	if len(keys) > 0 {
		var err error
		if value, err = s.lookup(keys); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	defer clear(raw)
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("credentials: decode %q: %s", strings.Join(keys, "."), jsonErrorKind(err))
	}
	return nil
}

// KeyRing reads an encryption key ring stored at path, in the form
//
//	{"active": "2026-1", "keys": {"2026-1": "<base64url>", "2025-1": "<base64url>"}}
//
// ready for encryption.NewCipher. Keeping data-encryption keys in the
// credentials file means one master key protects them, and rotation is an
// edit: add a key, make it active, run the re-encryption, and remove the old
// key only once a count of rows not on the active key is 0 (docs/ENCRYPTION.md).
func (s Store) KeyRing(keys ...string) (*encryption.KeyRing, error) {
	var ring struct {
		Active string            `json:"active"`
		Keys   map[string]string `json:"keys"`
	}
	if err := s.Decode(&ring, keys...); err != nil {
		return nil, err
	}
	return encryption.ParseKeyRing(ring.Active, ring.Keys)
}

// Require checks that every dotted path (for example "stripe.api_key") exists
// and is a non-empty string, number or boolean value or a non-empty
// object/array, and reports all that are not at once.
func (s Store) Require(paths ...string) error {
	var missing []string
	for _, dotted := range paths {
		value, err := s.lookup(strings.Split(dotted, "."))
		if err != nil || isEmpty(value) {
			missing = append(missing, dotted)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("%w in %s credentials: %s", ErrMissing, s.content().env, strings.Join(missing, ", "))
}

func isEmpty(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case map[string]any:
		return len(v) == 0
	case []any:
		return len(v) == 0
	}
	return false
}

func jsonType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case json.Number, float64:
		return "a number"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return "an unknown type"
}

func jsonErrorKind(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return "field " + typeErr.Field + " has the wrong type"
	}
	return "invalid structure"
}

// A Store never reveals its content through fmt, slog or JSON.

// GoString implements fmt.GoStringer.
func (s Store) GoString() string { return s.redacted() }

// Format implements fmt.Formatter so every verb prints the redacted form.
func (s Store) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.redacted())) }

// LogValue implements slog.LogValuer.
func (s Store) LogValue() slog.Value { return slog.StringValue(s.redacted()) }

// MarshalJSON prints the redacted form.
func (s Store) MarshalJSON() ([]byte, error) { return json.Marshal(s.redacted()) }

func (s Store) redacted() string {
	return "credentials.Store(" + s.content().env + ", [REDACTED])"
}
