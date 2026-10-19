package deploy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
)

// DefaultSecretsPath is the local secrets file; `forge deploy init` creates
// it 0600 and adds it to .gitignore and .dockerignore.
const DefaultSecretsPath = ".forge/secrets"

// minRedactLength is the shortest secret value replaced in output. Shorter
// values would redact unrelated text; such values are not credentials.
const minRedactLength = 4

// Redacted replaces secret values in printed output.
const Redacted = "[REDACTED]"

var passthroughPattern = regexp.MustCompile(`^\$(\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*)$`)

// Secrets are the values of .forge/secrets. They are never printed: use
// [Secrets.Redactor] for any output that could contain them.
type Secrets struct {
	values map[string]string
}

// LoadSecrets reads the dotenv-like secrets file at path.
//
//   - NAME=value sets a literal value; one pair of matching quotes is
//     removed and nothing inside is interpreted.
//   - NAME=$OTHER or NAME=${OTHER} copies OTHER from the local environment
//     (lookup), so values can come from a password manager or CI secret
//     store without being written to disk. A missing variable is an error.
//   - Blank lines and lines starting with # are ignored.
//
// There is deliberately no command substitution. The file must not be
// readable by group or others. Errors name the line or variable, never a
// value. A missing file yields an empty set; [Secrets.Require] then reports
// the names the configuration needs.
func LoadSecrets(path string, lookup func(string) (string, bool)) (Secrets, error) {
	secrets := Secrets{values: map[string]string{}}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return secrets, nil
	}
	if err != nil {
		return secrets, fmt.Errorf("secrets file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return secrets, fmt.Errorf("secrets file %s is accessible by other users (mode %04o); run: chmod 600 %s", path, info.Mode().Perm(), path)
	}
	content, err := os.ReadFile(path) // #nosec G304 -- the operator names the secrets file.
	if err != nil {
		return secrets, fmt.Errorf("secrets file: %w", err)
	}
	number := 0
	for line := range strings.Lines(string(content)) {
		number++
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		key = strings.TrimSpace(key)
		if !ok || !envKeyPattern.MatchString(key) {
			return secrets, fmt.Errorf("%s line %d: expected NAME=VALUE", path, number)
		}
		if _, dup := secrets.values[key]; dup {
			return secrets, fmt.Errorf("%s line %d: %s is defined twice", path, number, key)
		}
		value = strings.TrimSpace(value)
		quoted := len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0]
		if quoted {
			value = value[1 : len(value)-1]
		} else if passthroughPattern.MatchString(value) {
			name := strings.Trim(value, "${}")
			resolved, found := lookup(name)
			if !found {
				return secrets, fmt.Errorf("%s line %d: %s takes its value from $%s, which is not set", path, number, key, name)
			}
			value = resolved
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return secrets, fmt.Errorf("%s line %d: the value of %s must be a single line", path, number, key)
		}
		secrets.values[key] = value
	}
	return secrets, nil
}

// NewSecrets builds a set from a map; tests and callers that already hold
// values use it.
func NewSecrets(values map[string]string) Secrets {
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return Secrets{values: copied}
}

// Get returns a secret value.
func (s Secrets) Get(name string) (string, bool) {
	value, ok := s.values[name]
	return value, ok
}

// Require reports every name in names that has no value.
func (s Secrets) Require(names []string) error {
	var missing []string
	for _, name := range names {
		if _, ok := s.values[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("secrets not defined in %s: %s", DefaultSecretsPath, strings.Join(missing, ", "))
	}
	return nil
}

// Redactor returns a redactor for every value in the set.
func (s Secrets) Redactor() *Redactor {
	var values []string
	for _, value := range s.values {
		if len(value) >= minRedactLength {
			values = append(values, value)
		}
	}
	// Longest first, so a secret containing another is replaced whole.
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	return &Redactor{values: values}
}

// Redactor replaces known secret values in text.
type Redactor struct {
	values []string
}

// Redact returns text with every secret value replaced by [Redacted].
func (r *Redactor) Redact(text string) string {
	if r == nil {
		return text
	}
	for _, value := range r.values {
		text = strings.ReplaceAll(text, value, Redacted)
	}
	return text
}
