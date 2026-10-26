package credentials

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/rocketinsidecorp/forge-go/encryption"
)

// Init creates the credentials of env under dir: a new random key in
// <env>.key (mode 0600) and an encrypted skeleton holding a data-encryption
// key ring for package encryption (active key "k1"). It refuses to replace an
// existing credentials file and never overwrites a key; when the key file
// already exists it is used, so a restored key can start a new file. It
// returns the paths of the files it created.
//
// Callers must keep the key file out of version control; `forge credentials
// init` and `forge new` add the .gitignore entry.
func Init(dir, env string) ([]string, error) {
	if err := ValidateEnvironment(env); err != nil {
		return nil, err
	}
	if dir == "" {
		dir = DefaultDir
	}
	var created []string
	noEnvironment := func(string) (string, bool) { return "", false }
	keys, _, err := LoadKeys(dir, env, noEnvironment)
	switch {
	case errors.Is(err, ErrKeyMissing):
		material, err := encryption.GenerateKey()
		if err != nil {
			return nil, err
		}
		key, err := NewKey(material)
		clear(material)
		if err != nil {
			return nil, err
		}
		keys = []encryption.Key{key}
		if err := WriteKeyFile(dir, env, keys, false); err != nil {
			return nil, err
		}
		created = append(created, filepath.Join(dir, KeyFileName(env)))
	case err != nil:
		return nil, err
	}
	material, err := encryption.GenerateKey()
	if err != nil {
		return created, err
	}
	sealed, err := Seal(env, skeleton(material), keys[0])
	clear(material)
	if err != nil {
		return created, err
	}
	if err := WriteFile(dir, env, sealed, false); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return created, fmt.Errorf("%s already exists", filepath.Join(dir, FileName(env)))
		}
		return created, err
	}
	return append(created, filepath.Join(dir, FileName(env))), nil
}

// skeleton is the document Init seals: a key ring whose only key, "k1", is
// material. The key is base64url text, which needs no JSON escaping.
func skeleton(material []byte) []byte {
	return fmt.Appendf(nil, "{\n  \"encryption\": {\n    \"active\": \"k1\",\n    \"keys\": {\n      \"k1\": %q\n    }\n  }\n}\n", encryption.EncodeKey(material))
}
