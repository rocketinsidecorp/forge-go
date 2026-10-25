package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const postgresURL = "postgres://u:p@localhost/db"

func TestGenerateAuthenticationRefusesProjectsItCannotUnderstand(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		setup func(t *testing.T) string
		want  string
	}{
		"no such directory": {func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") }, "go.mod not found"},
		"no go.mod":         {func(t *testing.T) string { return t.TempDir() }, "go.mod not found"},
		"go.mod without a module": {func(t *testing.T) string {
			root := resourceProject(t, postgresURL)
			writeFile(t, filepath.Join(root, "go.mod"), "go 1.27.1\n")
			return root
		}, "go.mod has no module declaration"},
		"broken environment file": {func(t *testing.T) string {
			root := resourceProject(t, postgresURL)
			writeFile(t, filepath.Join(root, ".env.development"), "bad line\n")
			return root
		}, "environment files could not be loaded"},
		"a database it cannot identify": {func(t *testing.T) string { return resourceProject(t, "oracle://u:p@localhost/db") }, "a database FORGE_DATABASE_URL does not identify"},
		"a pages directory that is a symlink": {func(t *testing.T) string {
			root := resourceProject(t, postgresURL)
			target := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "app", "adapters"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, "app", "adapters", "htmlauth")); err != nil {
				t.Skipf("symlinks are not available: %v", err)
			}
			return root
		}, "refusing to inspect through symlink"},
		"app is a file": {func(t *testing.T) string {
			root := resourceProject(t, postgresURL)
			writeFile(t, filepath.Join(root, "app"), "not a directory")
			return root
		}, "the pages need the application's views package"},
		"a name the pages would redeclare": {func(t *testing.T) string {
			root := resourceProject(t, postgresURL)
			// Existing views.go that already declares what the base declares.
			writeFile(t, filepath.Join(root, "app", "views", "other.go"), "package views\n\nfunc New() {}\n")
			return root
		}, "already declares"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := tc.setup(t)
			var before []string
			if info, err := os.Stat(root); err == nil && info.IsDir() {
				before = snapshotTree(t, root)
			}
			_, err := runAuthentication(root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if before != nil && !slices.Equal(before, snapshotTree(t, root)) {
				t.Error("a refused run wrote files")
			}
		})
	}
}

func TestGenerateAuthenticationFollowsTheApplicationsLocalesAndCommandName(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, postgresURL)
	// Another locale in the application brings that locale's page text; a
	// locale Forge does not ship translations for is left alone.
	writeFile(t, filepath.Join(root, "config", "locales", "es.json"), `{"es":{}}`)
	writeFile(t, filepath.Join(root, "config", "locales", "fr.json"), `{"fr":{}}`)
	// A command directory that is not a valid application name.
	if err := os.Rename(filepath.Join(root, "cmd", "shop"), filepath.Join(root, "cmd", "Shop_App")); err != nil {
		t.Fatal(err)
	}
	output, err := runAuthentication(root, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, filepath.Join("config", "locales", "authentication.es.json")) {
		t.Errorf("the Spanish page text is not planned:\n%s", output)
	}
	if strings.Contains(output, "authentication.fr.json") || strings.Contains(output, "authentication.pt-BR.json") {
		t.Errorf("a locale the application lacks (or Forge does not ship) is planned:\n%s", output)
	}
	if !strings.Contains(output, filepath.Join("cmd", "Shop_App", "main.go")) {
		t.Errorf("the wiring does not name the application's command:\n%s", output)
	}
}

func TestGenerateAuthenticationReportsAFailingOutput(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, postgresURL)
	err := runGenerateAuthentication([]string{"--dry-run"}, failingWriter{}, root, nil)
	if err == nil || err != errBrokenPipe {
		t.Errorf("err = %v", err)
	}
}
