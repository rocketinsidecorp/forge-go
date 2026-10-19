package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecrets(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func env(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestLoadSecretsParsesLiteralsAndPassthrough(t *testing.T) {
	path := writeSecrets(t, `# comment

PLAIN=s3cr3t-value
export QUOTED="has $HOME and spaces"
SINGLE='$NOT_EXPANDED'
FROM_ENV=$LOCAL_TOKEN
BRACED=${LOCAL_TOKEN}
EMBEDDED=prefix$LOCAL_TOKEN
EMPTY=
`, 0o600)
	secrets, err := LoadSecrets(path, env(map[string]string{"LOCAL_TOKEN": "tok-123456"}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"PLAIN": "s3cr3t-value", "QUOTED": "has $HOME and spaces", "SINGLE": "$NOT_EXPANDED",
		"FROM_ENV": "tok-123456", "BRACED": "tok-123456", "EMBEDDED": "prefix$LOCAL_TOKEN", "EMPTY": "",
	}
	for key, value := range want {
		if got, ok := secrets.Get(key); !ok || got != value {
			t.Errorf("%s = %q, %v; want %q", key, got, ok, value)
		}
	}
	if err := secrets.Require([]string{"PLAIN", "MISSING_ONE", "MISSING_TWO"}); err == nil || !strings.Contains(err.Error(), "MISSING_ONE, MISSING_TWO") {
		t.Fatalf("Require error = %v", err)
	}
}

func TestLoadSecretsFailsClosedWithoutEchoingValues(t *testing.T) {
	cases := map[string]struct {
		content string
		mode    os.FileMode
		env     map[string]string
		want    string
	}{
		"group readable":  {"A=topsecretvalue\n", 0o640, nil, "chmod 600"},
		"world readable":  {"A=topsecretvalue\n", 0o604, nil, "chmod 600"},
		"missing env var": {"A=$NOT_SET\n", 0o600, nil, "$NOT_SET, which is not set"},
		"duplicate":       {"A=topsecretvalue\nA=other\n", 0o600, nil, "defined twice"},
		"bad line":        {"topsecretvalue\n", 0o600, nil, "line 1: expected NAME=VALUE"},
		"bad key":         {"A-B=topsecretvalue\n", 0o600, nil, "expected NAME=VALUE"},
		"multiline env":   {"A=$MULTI\n", 0o600, map[string]string{"MULTI": "topsecret\nvalue"}, "single line"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadSecrets(writeSecrets(t, tc.content, tc.mode), env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "topsecret") {
				t.Fatalf("error echoes a secret value: %v", err)
			}
		})
	}
	secrets, err := LoadSecrets(filepath.Join(t.TempDir(), "absent"), env(nil))
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if err := secrets.Require(nil); err != nil {
		t.Fatal(err)
	}
}

func TestRedactor(t *testing.T) {
	secrets := NewSecrets(map[string]string{
		"URL":   "postgres://u:pa55word@db/x",
		"PASS":  "pa55word",
		"SHORT": "abc",
	})
	redactor := secrets.Redactor()
	got := redactor.Redact("connect postgres://u:pa55word@db/x then pa55word; abc stays")
	if got != "connect [REDACTED] then [REDACTED]; abc stays" {
		t.Fatalf("Redact = %q", got)
	}
	var nilRedactor *Redactor
	if nilRedactor.Redact("x") != "x" {
		t.Fatal("nil redactor changed text")
	}
}

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"plain":                 "plain",
		"a b":                   "'a b'",
		"it's":                  `'it'\''s'`,
		"":                      "''",
		"$(rm -rf /)":           "'$(rm -rf /)'",
		"label=forge.role=web":  "label=forge.role=web",
		"{{json .}}":            "'{{json .}}'",
		"semi;colon":            "'semi;colon'",
		"/usr/local/bin/x":      "/usr/local/bin/x",
		"back`tick`":            "'back`tick`'",
		"new\nline":             "'new\nline'",
		"--name=a,b@c%d+e:f.g/": "--name=a,b@c%d+e:f.g/",
	}
	for input, want := range cases {
		if got := Quote(input); got != want {
			t.Errorf("Quote(%q) = %s, want %s", input, got, want)
		}
	}
}
