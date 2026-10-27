package forge_test

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The database never generates ids: UUIDv7 ids come from Go (uuid.New) on
// every backend, so no migration of the framework, an example or a template
// may default a column to uuidv7() (PostgreSQL 18 only) or gen_random_uuid()
// (version 4). examples/basic-api kept one until audit finding N3-03.
var databaseIDDefault = regexp.MustCompile(`(?i)\bdefault\s+\(?\s*(uuidv7|gen_random_uuid|uuid_generate_v[0-9]+)\s*\(`)

func TestNoMigrationDefaultsAnIDInTheDatabase(t *testing.T) {
	t.Parallel()
	checked := 0
	err := fs.WalkDir(os.DirFS("."), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "vendor" || entry.Name() == "node_modules") {
			return fs.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".sql") {
			return nil
		}
		content, err := os.ReadFile(path) // #nosec G304 -- a file of this repository found by the walk
		if err != nil {
			return err
		}
		checked++
		if found := databaseIDDefault.FindString(string(content)); found != "" {
			t.Errorf("%s has %q: ids are generated in Go, never by the database", path, found)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 20 {
		t.Fatalf("only %d SQL files were checked: the walk is broken", checked)
	}
	for _, bad := range []string{"id uuid PRIMARY KEY DEFAULT uuidv7()", "id uuid DEFAULT gen_random_uuid() NOT NULL", "id uuid default (uuidv7())"} {
		if !databaseIDDefault.MatchString(bad) {
			t.Errorf("the rule misses %q", bad)
		}
	}
}
