package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A generator never writes through a path that is not what it expects: a
// file where a directory belongs makes it stop with an error and leave the
// project exactly as it was.
func TestGeneratorsLeaveTheProjectUntouchedWhenALayoutIsNotWhatTheyExpect(t *testing.T) {
	t.Parallel()
	file := func(path string) func(t *testing.T, root string) {
		return func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, path), "a file where a directory belongs")
		}
	}
	cases := map[string]struct {
		prepare func(t *testing.T, root string)
		args    []string
	}{
		"view with app/views as a file":              {file("app/views"), []string{"view", "welcome"}},
		"view with app/views/pages as a file":        {file("app/views/pages"), []string{"view", "welcome"}},
		"view with config/locales as a file":         {file("config/locales"), []string{"view", "welcome"}},
		"view with app as a file":                    {file("app"), []string{"view", "welcome"}},
		"job with app as a file":                     {file("app"), []string{"job", "nightly_report"}},
		"job with app/jobs as a file":                {file("app/jobs"), []string{"job", "nightly_report"}},
		"dry run of a view with app/views as a file": {file("app/views"), []string{"view", "welcome", "--dry-run"}},
		"dry run of a job with app/jobs as a file":   {file("app/jobs"), []string{"job", "nightly_report", "--dry-run"}},
		"migration with db as a file":                {func(t *testing.T, root string) { _ = os.RemoveAll(filepath.Join(root, "db")); file("db")(t, root) }, []string{"migration", "add_prices"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
			writeFile(t, filepath.Join(root, "db", "migrations", ".keep"), "")
			tc.prepare(t, root)
			before := snapshotTree(t, root)
			var out bytes.Buffer
			err := runGenerateAtTime(tc.args, &out, root, generateNow)
			if err == nil {
				t.Fatalf("the generator wrote through a hostile layout:\n%s", out.String())
			}
			if after := snapshotTree(t, root); !slices.Equal(before, after) {
				t.Errorf("a failed generator changed the project:\nbefore %v\nafter  %v", before, after)
			}
			if strings.Contains(out.String(), "create ") && !strings.Contains(out.String(), "would") {
				t.Errorf("it reported files it did not create:\n%s", out.String())
			}
		})
	}
}

func TestApplyGeneratedFilesRefusesWhatItCannotInspectOrOpen(t *testing.T) {
	t.Parallel()
	// A root that does not exist.
	if err := applyGeneratedFiles(filepath.Join(t.TempDir(), "gone"), nil, nil); err == nil {
		t.Error("a missing root was accepted")
	}
	// A directory name the system cannot even look at (not "does not exist").
	root := t.TempDir()
	long := strings.Repeat("d", 300)
	err := applyGeneratedFiles(root, []string{filepath.Join("db", long)}, []generatedFile{{path: filepath.Join("db", long, "x.sql"), content: "x"}})
	if err == nil {
		t.Fatal("an unusable directory name was accepted")
	}
	// The directories made before the failure are taken back.
	if entries, readErr := os.ReadDir(root); readErr != nil || len(entries) != 0 {
		t.Errorf("the root holds %d entries after the failure (%v)", len(entries), readErr)
	}
}
