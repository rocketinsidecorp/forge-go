package scaffold_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/credentials"
	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

func TestGenerateCreatesDevelopmentCredentials(t *testing.T) {
	for _, database := range []string{"postgresql", "sqlite", "none"} {
		t.Run(database, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "shop")
			paths, err := scaffold.Generate(scaffold.Options{Name: "shop", Dir: dir, Database: database})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"config/credentials/development.json.enc", "config/credentials/development.key"} {
				if !contains(paths, filepath.FromSlash(want)) {
					t.Fatalf("missing %s in %v", want, paths)
				}
			}
			credDir := filepath.Join(dir, "config", "credentials")
			if runtime.GOOS != "windows" {
				if info, err := os.Stat(filepath.Join(credDir, "development.key")); err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("key file: %v, %v; want mode 0600", info, err)
				}
			}
			// The generated files work: the key opens the file and holds an
			// encryption key ring.
			store, err := credentials.Open("development", credentials.Options{Dir: credDir, Lookup: func(string) (string, bool) { return "", false }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.KeyRing("encryption"); err != nil {
				t.Fatalf("skeleton lacks an encryption key ring: %v", err)
			}
			// The key stays out of git and out of the Docker build context.
			assertContains(t, dir, map[string][]string{
				".gitignore":    {"/config/credentials/*.key"},
				".dockerignore": {"config/credentials/*.key"},
				"README.md":     {"forge credentials edit", "FORGE_MASTER_KEY"},
			})
			key := strings.TrimSpace(readFile(t, filepath.Join(credDir, "development.key")))
			walkErr := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() || filepath.Base(path) == "development.key" {
					return nil
				}
				if data, readErr := os.ReadFile(path); readErr == nil && strings.Contains(string(data), key) {
					t.Errorf("the credentials key appears in %s", path)
				}
				return nil
			})
			if walkErr != nil {
				t.Fatal(walkErr)
			}
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
