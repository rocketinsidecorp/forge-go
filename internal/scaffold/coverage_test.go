package scaffold_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

func TestAuthenticationTranslationIsShippedForEveryLocaleOnly(t *testing.T) {
	t.Parallel()
	for _, locale := range scaffold.Locales {
		file, err := scaffold.AuthenticationTranslation(locale)
		want := filepath.Join("config", "locales", "authentication."+locale+".json")
		if err != nil || file.Path != want || !strings.Contains(string(file.Content), `"`+locale+`"`) {
			t.Errorf("%s: %q, %v", locale, file.Path, err)
		}
	}
	for _, locale := range []string{"fr", "", "EN", "../en"} {
		if file, err := scaffold.AuthenticationTranslation(locale); err == nil || file.Path != "" || !strings.Contains(err.Error(), "no authentication translations") {
			t.Errorf("%q: %+v, %v", locale, file, err)
		}
	}
}

func TestI18nBaseIsTheTranslationsPackageOfForgeNew(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "shop")
	if _, err := scaffold.Generate(scaffold.Options{Name: "shop", Module: "example.com/shop", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	files, err := scaffold.I18nBase("shop", "example.com/shop")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, file := range files {
		paths[filepath.ToSlash(file.Path)] = true
		generated, err := os.ReadFile(filepath.Join(dir, file.Path))
		if err != nil {
			t.Fatal(err)
		}
		if string(generated) != string(file.Content) {
			t.Errorf("%s differs between forge new and I18nBase", file.Path)
		}
	}
	for _, want := range []string{"config/locales/locales.go", "config/locales/locales_test.go", "config/locales/en.json"} {
		if !paths[want] {
			t.Errorf("I18nBase lacks %s: %v", want, paths)
		}
	}
	if len(paths) != 3 {
		t.Errorf("I18nBase returned %v", paths)
	}
}

func TestBaseGeneratorsRejectInvalidNamesAndModules(t *testing.T) {
	t.Parallel()
	calls := map[string]func(name, module string) error{
		"ViewBase": func(n, m string) error { _, err := scaffold.ViewBase(n, m); return err },
		"I18nBase": func(n, m string) error { _, err := scaffold.I18nBase(n, m); return err },
		"AuthenticationFiles": func(n, m string) error {
			_, err := scaffold.AuthenticationFiles(scaffold.Options{Name: n, Module: m, Database: "sqlite"})
			return err
		},
	}
	for name, call := range calls {
		if err := call("Not Valid", "example.com/x"); err == nil || !strings.Contains(err.Error(), "application name") {
			t.Errorf("%s with a bad name: %v", name, err)
		}
		if err := call("shop", "example.com/../x"); err == nil || !strings.Contains(err.Error(), "invalid Go module path") {
			t.Errorf("%s with a bad module: %v", name, err)
		}
		if err := call("shop", "example.com/shop"); err != nil {
			t.Errorf("%s with valid input: %v", name, err)
		}
	}
}

func TestGenerateFailsCleanlyWhenTheTargetCannotBeUsed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	file := filepath.Join(root, "a-file")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		opts scaffold.Options
		want string
	}{
		"invalid name":           {scaffold.Options{Name: "Bad Name"}, "application name"},
		"invalid module":         {scaffold.Options{Name: "shop", Module: "bad module"}, "invalid Go module path"},
		"unknown database":       {scaffold.Options{Name: "shop", Database: "oracle"}, "oracle"},
		"unknown locale":         {scaffold.Options{Name: "shop", Locale: "fr"}, "unsupported locale"},
		"target is a file":       {scaffold.Options{Name: "shop", Dir: file}, ""},
		"target below a file":    {scaffold.Options{Name: "shop", Dir: filepath.Join(file, "shop")}, ""},
		"target is not a folder": {scaffold.Options{Name: "shop", Dir: filepath.Join(file, "x", "y")}, ""},
	} {
		dir := tc.opts.Dir
		if dir == "" {
			tc.opts.Dir = filepath.Join(root, strings.ReplaceAll(name, " ", "-"))
			dir = tc.opts.Dir
		}
		paths, err := scaffold.Generate(tc.opts)
		if err == nil || paths != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: paths %v, err %v, want %q", name, paths, err, tc.want)
		}
		if info, statErr := os.Stat(dir); statErr == nil && dir != file {
			t.Errorf("%s: %s was left behind (%v)", name, dir, info.Mode())
		}
	}
	if content, err := os.ReadFile(file); err != nil || string(content) != "data" {
		t.Errorf("the existing file changed: %q, %v", content, err)
	}
}

func TestGenerateAcceptsAnExistingEmptyDirectoryAndKeepsIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // exists and is empty: used as it is
	paths, err := scaffold.Generate(scaffold.Options{Name: "shop", Dir: dir})
	if err != nil || len(paths) == 0 {
		t.Fatalf("paths %d, err %v", len(paths), err)
	}
	for _, relative := range paths {
		if _, err := os.Stat(filepath.Join(dir, relative)); err != nil {
			t.Errorf("%s was reported but is missing: %v", relative, err)
		}
	}
}
