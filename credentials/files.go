package credentials

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/rocketinsidecorp/forge-go/encryption"
)

const (
	// DefaultDir is where credentials live, relative to the application root.
	DefaultDir = "config/credentials"
	// EnvMasterKey names the environment variable that carries the key.
	EnvMasterKey = "FORGE_MASTER_KEY"

	fileSuffix = ".json.enc"
	keySuffix  = ".key"
	maxKeyFile = 4096
)

var (
	// ErrNotFound reports that no credentials file exists for the environment.
	ErrNotFound = errors.New("credentials: file not found")
	// ErrKeyMissing reports that neither FORGE_MASTER_KEY nor a key file exists.
	ErrKeyMissing = errors.New("credentials: no key available (set " + EnvMasterKey + " or create the environment's .key file)")
	// ErrKeyPermissions reports a key file readable by group or others.
	ErrKeyPermissions = errors.New("credentials: key file must not be accessible by group or others (chmod 600)")
	// ErrDirectoryPermissions reports a credentials directory that group or
	// others can write, so they could replace the key file.
	ErrDirectoryPermissions = errors.New("credentials: directory must not be writable by group or others (chmod 700)")
	// ErrSymlink reports a key or credentials file that is a symbolic link.
	// Neither is followed, even to a file inside the directory: whoever can
	// plant a link there could point it at a key they control.
	ErrSymlink = errors.New("credentials: symbolic links are not accepted")
)

// FileName returns the credentials file name of env, such as production.json.enc.
func FileName(env string) string { return env + fileSuffix }

// KeyFileName returns the key file name of env, such as production.key.
func KeyFileName(env string) string { return env + keySuffix }

// KeySource says where LoadKeys found the key.
type KeySource struct {
	// Environment is true when the key came from FORGE_MASTER_KEY.
	Environment bool
	// Path is the key file, empty when the key came from the environment.
	Path string
}

func (s KeySource) String() string {
	if s.Environment {
		return EnvMasterKey
	}
	return s.Path
}

// LoadKeys reads the keys for env: FORGE_MASTER_KEY when set (through lookup,
// os.LookupEnv when nil), otherwise <dir>/<env>.key. The first key is active;
// the others only decrypt. Key files that group or others can read, key
// files in a directory that group or others can write, symbolic links
// (ErrSymlink) and non-regular files (a FIFO, a device) are refused. With neither source the error is
// ErrKeyMissing.
func LoadKeys(dir, env string, lookup func(string) (string, bool)) ([]encryption.Key, KeySource, error) {
	if err := ValidateEnvironment(env); err != nil {
		return nil, KeySource{}, err
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if value, ok := lookup(EnvMasterKey); ok && strings.TrimSpace(value) != "" {
		keys, err := ParseKeys(value)
		if err != nil {
			return nil, KeySource{}, fmt.Errorf("%s: %w", EnvMasterKey, err)
		}
		return keys, KeySource{Environment: true}, nil
	}
	if dir == "" {
		dir = DefaultDir
	}
	path := filepath.Join(dir, KeyFileName(env))
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, KeySource{}, ErrKeyMissing
		}
		return nil, KeySource{}, err
	}
	defer func() { _ = root.Close() }()
	if err := checkPrivateDir(root, dir); err != nil {
		return nil, KeySource{}, err
	}
	file, err := openNoFollow(root, KeyFileName(env))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, KeySource{}, ErrKeyMissing
	}
	if err != nil {
		return nil, KeySource{}, fmt.Errorf("credentials: open key file: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, KeySource{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, KeySource{}, fmt.Errorf("credentials: %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, KeySource{}, fmt.Errorf("%w: %s is %04o", ErrKeyPermissions, path, info.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(file, maxKeyFile+1))
	if err != nil {
		return nil, KeySource{}, err
	}
	if len(data) > maxKeyFile {
		return nil, KeySource{}, fmt.Errorf("credentials: key file %s is too large", path)
	}
	keys, err := ParseKeys(string(data))
	clear(data)
	if err != nil {
		return nil, KeySource{}, fmt.Errorf("%s: %w", path, err)
	}
	return keys, KeySource{Path: path}, nil
}

// ReadFile reads the encrypted file of env from dir. A missing file is
// ErrNotFound; a symbolic link is refused (ErrSymlink).
func ReadFile(dir, env string) ([]byte, error) {
	if err := ValidateEnvironment(env); err != nil {
		return nil, err
	}
	if dir == "" {
		dir = DefaultDir
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return readLimited(openNoFollow(root, FileName(env)))
}

// openNoFollow opens name in root like openNonblocking, but refuses a
// symbolic link (ErrSymlink): os.Root follows links that stay inside the
// directory and ignores O_NOFOLLOW, so name is checked with Lstat first, and
// the opened file must be the one Lstat saw, so a link swapped in between
// the two calls is refused too.
func openNoFollow(root *os.Root, name string) (*os.File, error) {
	seen, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if seen.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", ErrSymlink, name)
	}
	file, err := openNonblocking(root, name)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !os.SameFile(seen, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s changed while it was opened", ErrSymlink, name)
	}
	return file, nil
}

// openNonblocking opens name in root for reading without blocking: opening a
// FIFO for reading waits for a writer unless O_NONBLOCK is set, which would
// hang before the regular-file check could refuse it. Reads of a regular file
// ignore the flag.
func openNonblocking(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|openNonblock, 0)
}

// checkPrivateDir refuses a credentials directory that group or others can
// write: they could replace the key file (and the encrypted file with one
// sealed under their key) even though the key file itself is 0600.
func checkPrivateDir(root *os.Root, dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := root.Stat(".")
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s is %04o", ErrDirectoryPermissions, dir, info.Mode().Perm())
	}
	return nil
}

func readLimited(file *os.File, err error) ([]byte, error) {
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("credentials: open %s: not a regular file", file.Name())
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("%w: file is too large", ErrFormat)
	}
	return data, nil
}

// WriteFile stores the encrypted file of env in dir, creating dir (0700) when
// needed. With replace it swaps the file atomically (temporary file in the
// same directory, fsync, rename), so readers see the old or the new file and
// never a partial one. Without replace it fails if the file exists.
func WriteFile(dir, env string, data []byte, replace bool) error {
	if err := ValidateEnvironment(env); err != nil {
		return err
	}
	return writeAtomic(dir, FileName(env), data, replace)
}

// WriteKeyFile stores keys (active first) as <dir>/<env>.key with mode 0600.
// dir is created with mode 0700 when missing; an existing directory that group
// or others can write is refused (ErrDirectoryPermissions). Without replace it
// fails if the file exists, so a key is never overwritten by accident.
func WriteKeyFile(dir, env string, keys []encryption.Key, replace bool) error {
	if err := ValidateEnvironment(env); err != nil {
		return err
	}
	if len(keys) == 0 || len(keys) > MaxKeys {
		return fmt.Errorf("credentials: between 1 and %d keys are required", MaxKeys)
	}
	root, dir, err := keyDir(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var text strings.Builder
	for _, key := range keys {
		text.WriteString(encryption.EncodeKey(key.Material))
		text.WriteByte('\n')
	}
	data := []byte(text.String())
	defer clear(data)
	return writeAtomicIn(root, dir, KeyFileName(env), data, replace)
}

// PrepareKeyDir makes dir ready to hold key files: it creates dir with mode
// 0700 when missing and refuses an existing directory that group or others
// can write (ErrDirectoryPermissions). WriteKeyFile does the same; call this
// first to fail before showing a new key to anyone.
func PrepareKeyDir(dir string) error {
	root, _, err := keyDir(dir)
	if err != nil {
		return err
	}
	return root.Close()
}

// keyDir creates and opens the key directory and checks it on the opened
// root, so the directory checked is the one written to.
func keyDir(dir string) (*os.Root, string, error) {
	if dir == "" {
		dir = DefaultDir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, dir, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, dir, err
	}
	if err := checkPrivateDir(root, dir); err != nil {
		_ = root.Close()
		return nil, dir, err
	}
	return root, dir, nil
}

func writeAtomic(dir, name string, data []byte, replace bool) error {
	if dir == "" {
		dir = DefaultDir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return writeAtomicIn(root, dir, name, data, replace)
}

// writeAtomicIn stores name in root (the directory dir) atomically.
func writeAtomicIn(root *os.Root, dir, name string, data []byte, replace bool) (err error) {
	if !replace {
		if _, statErr := root.Lstat(name); statErr == nil {
			return fmt.Errorf("%w: %s", fs.ErrExist, filepath.Join(dir, name))
		}
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	temp := "." + name + "." + hex.EncodeToString(suffix) + ".tmp"
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = root.Remove(temp)
		}
	}()
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if !replace {
		// Link refuses to replace an existing file, which closes the window
		// between the check above and the final name appearing.
		if err = root.Link(temp, name); err != nil {
			return err
		}
		_ = root.Remove(temp)
		return syncDir(root)
	}
	if err = root.Rename(temp, name); err != nil {
		return err
	}
	return syncDir(root)
}

// syncDir makes the rename durable. It is best effort: some platforms cannot
// fsync a directory.
func syncDir(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return nil
	}
	_ = d.Sync()
	return d.Close()
}
