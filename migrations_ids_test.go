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

// databaseIDDefaultHistory are the files that name such a default and may keep
// doing so: a released migration is never edited, so create_tasks keeps the
// default it shipped with, and the migration that drops it says how to put it
// back in its down file. Everything else must be free of the rule.
var databaseIDDefaultHistory = map[string]bool{
	"examples/basic-api/db/migrations/20260926000000_create_tasks.up.sql":           true,
	"examples/basic-api/db/migrations/20261027000000_drop_task_id_default.down.sql": true,
}

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
		if found := databaseIDDefault.FindString(string(content)); found != "" && !databaseIDDefaultHistory[path] {
			t.Errorf("%s has %q: ids are generated in Go, never by the database", path, found)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The history list names files that exist and still match the rule, so it
	// cannot go stale and hide a new violation.
	for path := range databaseIDDefaultHistory {
		if content, err := os.ReadFile(path); err != nil || !databaseIDDefault.Match(content) { // #nosec G304 -- a fixed path of this repository
			t.Errorf("%s is listed as historical but is missing or no longer defaults an id: %v", path, err)
		}
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
