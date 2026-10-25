package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const completeLocales = `{"en": {"greeting": "Hello"}}`

func TestRunI18nChecksTheCurrentApplication(t *testing.T) {
	// Not parallel: the working directory.
	root := i18nProject(t, map[string]string{
		"config/locales/en.json":    `{"en": {"greeting": "Hello", "farewell": "Bye"}}`,
		"config/locales/pt-BR.json": `{"pt-BR": {"greeting": "Olá"}}`,
		"app/views/home.gohtml":     `{{t "greeting"}}`,
	})
	t.Chdir(root)
	for _, args := range [][]string{{"check"}, {"check", "--json"}} {
		var out bytes.Buffer
		err := runI18n(args, &out)
		if !errors.Is(err, errTranslationsMissing) {
			t.Errorf("%v: err = %v, want the missing-translations error", args, err)
		}
		if !strings.Contains(out.String(), "farewell") {
			t.Errorf("%v: the report does not name the missing key:\n%s", args, out.String())
		}
	}
	if err := runI18n([]string{"check"}, failingWriter{}); err != errBrokenPipe {
		t.Errorf("a failing output: %v", err)
	}
	if err := runI18n([]string{"check", "--json"}, failingWriter{}); err != errBrokenPipe {
		t.Errorf("a failing JSON output: %v", err)
	}
	// A project whose translations are complete passes.
	writeFile(t, filepath.Join(root, "config", "locales", "pt-BR.json"), `{"pt-BR": {"greeting": "Olá", "farewell": "Tchau"}}`)
	var out bytes.Buffer
	if err := runI18n([]string{"check"}, &out); err != nil || !strings.Contains(out.String(), "pt-BR: complete") {
		t.Errorf("complete translations: %v\n%s", err, out.String())
	}
}

func TestRunI18nRejectsMalformedCommandLines(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"scan"}, {"check", "--yaml"}, {"check", "--json", "extra"}} {
		var out bytes.Buffer
		if err := runI18n(args, &out); err == nil || !strings.Contains(err.Error(), "usage: forge i18n check [--json]") {
			t.Errorf("forge i18n %v: %v", args, err)
		}
	}
}

func TestCheckTranslationsReportsAnUnusableProject(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		root func(t *testing.T) string
		want string
	}{
		"no such directory": {func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") }, "go.mod not found"},
		"no go.mod":         {func(t *testing.T) string { return t.TempDir() }, "go.mod not found"},
		"no locales":        {func(t *testing.T) string { return i18nProject(t, nil) }, "config/locales"},
		"locales is a file": {func(t *testing.T) string {
			return i18nProject(t, map[string]string{"config/locales": "a file"})
		}, "config/locales"},
		"no translation files": {func(t *testing.T) string {
			return i18nProject(t, map[string]string{"config/locales/README.md": "notes", "config/locales/.hidden.json": "{}"})
		}, "has no translation files"},
		"a misspelled locale": {func(t *testing.T) string {
			return i18nProject(t, map[string]string{"config/locales/EN.json": completeLocales})
		}, "EN.json"},
		"a file that does not load": {func(t *testing.T) string {
			return i18nProject(t, map[string]string{"config/locales/en.json": `{"en": `})
		}, "en.json"},
	} {
		if report, err := checkTranslations(tc.root(t)); err == nil || report != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: report %v, err %v, want %q", name, report, err, tc.want)
		}
	}
}

func TestScanReferencesSkipsWhatItShouldNotRead(t *testing.T) {
	t.Parallel()
	root := i18nProject(t, map[string]string{
		"config/locales/en.json":  `{"en": {"used": "x", "linked": "y", "huge": "z", "skipped": "s"}}`,
		"app/a.gohtml":            `{{t "used"}}`,
		"node_modules/n/b.gohtml": `{{t "skipped"}}`,
	})
	// A file beyond the size limit is not scanned, a symlink is not followed.
	writeFile(t, filepath.Join(root, "app", "huge.go"), "package app\n// "+strings.Repeat("x", maxScannedFileBytes)+"\nvar _ = i18n.T(ctx, \"huge\")\n")
	outside := filepath.Join(t.TempDir(), "outside.gohtml")
	writeFile(t, outside, `{{t "linked"}}`)
	if err := os.Symlink(outside, filepath.Join(root, "app", "link.gohtml")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	report, err := checkTranslations(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Referenced != 1 {
		t.Errorf("referenced %d keys, want only %q", report.Referenced, "used")
	}
	for _, key := range []string{"linked", "huge", "skipped"} {
		found := false
		for _, unused := range report.Unused {
			found = found || unused == key
		}
		if !found {
			t.Errorf("%s was read although it should have been skipped (unused: %v)", key, report.Unused)
		}
	}
}
