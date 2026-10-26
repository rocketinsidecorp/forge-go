package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// vendoredApp is a directory shaped like an application with a database: a
// go.mod, db/migrations and, optionally, the vendor directory forge new --forge-path creates.
func vendoredApp(t *testing.T, vendor bool) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module shop\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirs := []string{filepath.Join("db", "migrations")}
	if vendor {
		dirs = append(dirs, "vendor")
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A generator can import framework packages the vendor directory lacks (a
// scaffold's pagination), so in a vendored application a successful run is
// followed by go mod vendor, and only then.
func TestGenerateRevendorsAVendoredApplication(t *testing.T) {
	// Not parallel: runGoStep is replaced.
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	t.Run("a vendored application", func(t *testing.T) {
		steps := recordGoSteps(t, "")
		root := vendoredApp(t, true)
		var out strings.Builder
		if err := runGenerateEnv([]string{"migration", "add_things"}, &out, root, nil, now); err != nil {
			t.Fatal(err)
		}
		if len(*steps) != 1 || strings.Join((*steps)[0], " ") != "mod vendor" {
			t.Fatalf("steps = %v, want one go mod vendor", *steps)
		}
		if !strings.Contains(out.String(), "run     go mod vendor") {
			t.Errorf("output does not say it vendored:\n%s", out.String())
		}
	})

	t.Run("an application without vendor", func(t *testing.T) {
		steps := recordGoSteps(t, "")
		if err := runGenerateEnv([]string{"migration", "add_things"}, &strings.Builder{}, vendoredApp(t, false), nil, now); err != nil {
			t.Fatal(err)
		}
		if len(*steps) != 0 {
			t.Fatalf("a go command ran without a vendor directory: %v", *steps)
		}
	})

	t.Run("a dry run", func(t *testing.T) {
		steps := recordGoSteps(t, "")
		if err := runGenerateEnv([]string{"migration", "add_things", "--dry-run"}, &strings.Builder{}, vendoredApp(t, true), nil, now); err != nil {
			t.Fatal(err)
		}
		if len(*steps) != 0 {
			t.Fatalf("a dry run vendored: %v", *steps)
		}
	})

	t.Run("a failed generator", func(t *testing.T) {
		steps := recordGoSteps(t, "")
		if err := runGenerateEnv([]string{"migration", "Not A Name"}, &strings.Builder{}, vendoredApp(t, true), nil, now); err == nil || !strings.Contains(err.Error(), "migration name") {
			t.Fatalf("an invalid name: %v", err)
		}
		if len(*steps) != 0 {
			t.Fatalf("a failed generator vendored: %v", *steps)
		}
	})

	t.Run("go mod vendor fails", func(t *testing.T) {
		recordGoSteps(t, "mod")
		root := vendoredApp(t, true)
		err := runGenerateEnv([]string{"migration", "add_things"}, &strings.Builder{}, root, nil, now)
		if err == nil || !strings.Contains(err.Error(), "the files were generated, but go mod vendor failed") || !strings.Contains(err.Error(), root) {
			t.Fatalf("err = %v, want the way to finish by hand", err)
		}
		if matches, _ := filepath.Glob(filepath.Join(root, "db", "migrations", "*_add_things.up.sql")); len(matches) != 1 {
			t.Errorf("the generated migration was removed: %v", matches)
		}
	})
}
