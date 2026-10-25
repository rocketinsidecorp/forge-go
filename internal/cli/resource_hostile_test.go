package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The resource generators read the project's migrations and Go files to avoid
// collisions. Anything unreadable or odd in there stops the generation with a
// message, or is skipped when it cannot matter, and nothing is half written.
func TestResourceGenerationReportsAnApplicationItCannotRead(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.sql"), "-- not part of the project\n")
	cases := map[string]struct {
		prepare func(t *testing.T, root string)
		args    []string
		want    string
	}{
		"go.mod without a module": {
			func(t *testing.T, root string) { writeFile(t, filepath.Join(root, "go.mod"), "go 1.27.1\n") },
			[]string{"resource", "post", "title:string", "--global"}, "no module declaration",
		},
		"no migrations directory": {
			func(t *testing.T, root string) { _ = os.RemoveAll(filepath.Join(root, "db")) },
			[]string{"resource", "post", "title:string", "--global"}, "db/migrations not found",
		},
		"migrations directory is a symlink": {
			func(t *testing.T, root string) {
				_ = os.RemoveAll(filepath.Join(root, "db", "migrations"))
				if err := os.Symlink(outside, filepath.Join(root, "db", "migrations")); err != nil {
					t.Skipf("symbolic links are not available: %v", err)
				}
			},
			[]string{"resource", "post", "title:string", "--global"}, "symlink",
		},
		"a migration that escapes the project": {
			func(t *testing.T, root string) {
				if err := os.Symlink(filepath.Join(outside, "secret.sql"), filepath.Join(root, "db", "migrations", "20260101000000_other.up.sql")); err != nil {
					t.Skipf("symbolic links are not available: %v", err)
				}
			},
			[]string{"resource", "post", "title:string", "--global"}, "read db/migrations/20260101000000_other.up.sql",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := resourceProject(t, postgresURL)
			tc.prepare(t, root)
			before := snapshotTree(t, root)
			var out bytes.Buffer
			err := runGenerateEnv(tc.args, &out, root, nil, generateNow)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if after := snapshotTree(t, root); !slices.Equal(before, after) {
				t.Errorf("a refused generation changed the project:\nbefore %v\nafter  %v", before, after)
			}
		})
	}

	// forge destroy reads the same directory.
	for name, prepare := range map[string]func(t *testing.T, root string){
		"no migrations directory": func(t *testing.T, root string) { _ = os.RemoveAll(filepath.Join(root, "db")) },
		"migrations directory is a symlink": func(t *testing.T, root string) {
			_ = os.RemoveAll(filepath.Join(root, "db", "migrations"))
			if err := os.Symlink(outside, filepath.Join(root, "db", "migrations")); err != nil {
				t.Skipf("symbolic links are not available: %v", err)
			}
		},
	} {
		root := resourceProject(t, postgresURL)
		prepare(t, root)
		var out bytes.Buffer
		if err := runDestroyEnv([]string{"resource", "post", "title:string", "--global"}, &out, root, nil, generateNow); err == nil {
			t.Errorf("forge destroy with %s succeeded", name)
		}
	}
}

// Go files in the target package that cannot be read or parsed do not stop a
// generation: they cannot declare a name the generated code would clash with
// in a way the compiler would not report itself.
func TestResourceGenerationSkipsGoFilesItCannotUse(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, postgresURL)
	domain := filepath.Join(root, "app", "domain")
	writeFile(t, filepath.Join(domain, "broken.go"), "package domain\n\nfunc (")
	writeFile(t, filepath.Join(domain, "notes.txt"), "not Go")
	if err := os.Mkdir(filepath.Join(domain, "dir.go"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "go.mod"), filepath.Join(domain, "link.go")); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "elsewhere.go"), "package domain\n\ntype Post struct{}\n")
	if err := os.Symlink(filepath.Join(outside, "elsewhere.go"), filepath.Join(domain, "escape.go")); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}
	var out bytes.Buffer
	if err := runGenerateEnv([]string{"resource", "post", "title:string", "--global"}, &out, root, nil, generateNow); err != nil {
		t.Fatalf("the generation stopped for files it cannot use: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "app", "domain", "post.go")); err != nil {
		t.Errorf("the resource was not generated: %v", err)
	}
}
