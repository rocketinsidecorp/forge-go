package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forge inspect reports a source tree it cannot read as incomplete and names
// the problem; it neither follows a link out of the project nor stops.
func TestInspectReportsSourceItCannotReadAsIncomplete(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.go"), "package secret\n\nconst Token = \"outside-the-project\"\n")
	cases := map[string]func(t *testing.T, root string){
		"a source root that loops": func(t *testing.T, root string) {
			if err := os.Symlink("app", filepath.Join(root, "app")); err != nil {
				t.Skipf("symbolic links are not available: %v", err)
			}
		},
		"a Go file that leads out of the project": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "app", "ok.go"), "package app\n")
			if err := os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(root, "app", "escape.go")); err != nil {
				t.Skipf("symbolic links are not available: %v", err)
			}
		},
		"a Go file that is a dangling link": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "app", "ok.go"), "package app\n")
			if err := os.Symlink(filepath.Join(outside, "missing.go"), filepath.Join(root, "app", "dangling.go")); err != nil {
				t.Skipf("symbolic links are not available: %v", err)
			}
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\ngo 1.27.1\nrequire github.com/rocketinsidecorp/forge-go v0.0.0\n")
			prepare(t, root)
			var output bytes.Buffer
			err := runInspectAt([]string{"--json"}, &output, root, nil)
			if !errors.Is(err, errInspectIncomplete) {
				t.Fatalf("err = %v, want an incomplete inspection\n%s", err, output.String())
			}
			var report projectInspection
			if err := json.Unmarshal(output.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report.Complete || !hasInspectCode(report.Diagnostics, "project.source_unreadable") {
				t.Errorf("the unreadable source was not disclosed: %+v", report)
			}
			if strings.Contains(output.String(), "outside-the-project") {
				t.Errorf("a file outside the project was read:\n%s", output.String())
			}
		})
	}
}
