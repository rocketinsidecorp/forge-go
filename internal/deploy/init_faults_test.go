package deploy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInitReportsWhatItCouldNotCreateAndWhatItDid(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("config directory blocked by a file", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		write(t, filepath.Join(root, "config"), "a file where the directory belongs")
		changed, err := Init(root, "shop")
		if err == nil || len(changed) != 0 {
			t.Fatalf("Init = %v, %v; want a failure with nothing created", changed, err)
		}
		if _, err := os.Stat(filepath.Join(root, ".forge")); err == nil {
			t.Error("secrets were written although the configuration could not be")
		}
	})

	t.Run("secrets directory blocked by a file", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		write(t, filepath.Join(root, ".forge"), "a file where the directory belongs")
		changed, err := Init(root, "shop")
		if err == nil {
			t.Fatal("Init succeeded")
		}
		if !slices.Equal(changed, []string{DefaultConfigPath}) {
			t.Errorf("changed = %v, want exactly the configuration that was written", changed)
		}
	})

	t.Run("ignore file unreadable", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".gitignore"), 0o750); err != nil { // a directory cannot be read as a file
			t.Fatal(err)
		}
		changed, err := Init(root, "shop")
		if err == nil {
			t.Fatal("Init succeeded")
		}
		if !slices.Equal(changed, []string{DefaultConfigPath, DefaultSecretsPath}) {
			t.Errorf("changed = %v", changed)
		}
		// The secrets file stays private even though a later step failed.
		info, statErr := os.Stat(filepath.Join(root, DefaultSecretsPath))
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("secrets file: %v %v", info, statErr)
		}
	})

	t.Run("ignore entries already present in any spelling", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		write(t, filepath.Join(root, ".gitignore"), "/.forge/secrets\n")
		write(t, filepath.Join(root, ".dockerignore"), "node_modules\n.forge/")
		changed, err := Init(root, "shop")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(changed, []string{DefaultConfigPath, DefaultSecretsPath}) {
			t.Errorf("changed = %v: an ignore file that already covers the path was edited", changed)
		}
		for file, want := range map[string]string{".gitignore": "/.forge/secrets\n", ".dockerignore": "node_modules\n.forge/"} {
			got, err := os.ReadFile(filepath.Join(root, file))
			if err != nil || string(got) != want {
				t.Errorf("%s = %q (%v), want it untouched", file, got, err)
			}
		}
	})

	t.Run("an ignore file without a final newline keeps its last line", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		write(t, filepath.Join(root, ".gitignore"), "build")
		if _, err := Init(root, "shop"); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(root, ".gitignore"))
		if err != nil || !strings.HasPrefix(string(got), "build\n# forge deploy\n.forge/secrets\n") {
			t.Errorf(".gitignore = %q (%v)", got, err)
		}
	})
}
