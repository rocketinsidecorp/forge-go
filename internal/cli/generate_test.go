package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateMigration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, _, err := generateMigration(root, "create_widgets", time.Now()); err == nil {
		t.Fatal("expected error outside an application with db/migrations")
	}
	if err := os.MkdirAll(filepath.Join(root, "db", "migrations"), 0o750); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.FixedZone("BRT", -3*3600))
	up, down, err := generateMigration(root, "create_widgets", now)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(up) != "20260926150000_create_widgets.up.sql" || filepath.Base(down) != "20260926150000_create_widgets.down.sql" {
		t.Fatalf("up=%s down=%s (version must be the UTC timestamp)", up, down)
	}
	// Same second: the version is bumped instead of colliding.
	second, _, err := generateMigration(root, "add_index", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(second), "20260926150001_") {
		t.Fatalf("second=%s", second)
	}
	for _, name := range []string{"CreateWidgets", "1widgets", "drop-table", "../escape", ""} {
		if _, _, err := generateMigration(root, name, now); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
}

func TestGenerateViewAddsAPageToTheSharedViewTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")

	// The application has no views package yet: the base `forge new` generates
	// comes with the first view.
	paths, err := generateView(root, "welcome")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join("app", "views", "pages", "welcome.gohtml"),
		filepath.Join("app", "views", "views.go"),
		filepath.Join("app", "views", "views_test.go"),
		filepath.Join("app", "views", "layouts", "application.gohtml"),
		filepath.Join("app", "views", "shared", "_flash.gohtml"),
	}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("generated paths = %v, want %v", paths, want)
	}
	page := readFile(t, filepath.Join(root, paths[0]))
	for _, fragment := range []string{`{{define "title"}}{{.Title}}{{end}}`, `{{define "content"}}`, "{{.Content}}"} {
		if !strings.Contains(page, fragment) {
			t.Fatalf("generated page is missing %q: %s", fragment, page)
		}
	}
	viewsGo := readFile(t, filepath.Join(root, paths[1]))
	if !strings.Contains(viewsGo, "//go:embed */*.gohtml") || !strings.Contains(viewsGo, "func New()") {
		t.Fatalf("views package does not embed the view tree: %s", viewsGo)
	}
	if layout := readFile(t, filepath.Join(root, paths[3])); !strings.Contains(layout, `{{block "content" .}}`) || !strings.Contains(layout, "<title>{{block \"title\" .}}shop{{end}}</title>") {
		t.Fatalf("layout = %s", layout)
	}

	// A second view, also in another directory, adds only its page and leaves the base alone.
	paths, err = generateView(root, "posts/archive")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != filepath.Join("app", "views", "posts", "archive.gohtml") {
		t.Fatalf("second view should only add its page, got %v", paths)
	}
	if after := readFile(t, filepath.Join(root, "app", "views", "views.go")); after != viewsGo {
		t.Fatal("second view generation modified the views package")
	}
	if _, err := generateView(root, "welcome"); err == nil {
		t.Fatal("existing view was overwritten")
	}
}

func TestGenerateViewValidatesNamesAndApplicationRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"", "Welcome", "../escape", "x-y", strings.Repeat("a", 65), "a/b/c", "layouts/home", "posts/", "/home", "_partial"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := generateView(root, name); err == nil {
				t.Fatalf("accepted invalid view name %q", name)
			}
		})
	}
	if _, err := generateView(root, "home"); err == nil {
		t.Fatal("generated a view outside an application")
	}
}

func TestGenerateViewRefusesUnknownViewsPackage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
	writeFile(t, filepath.Join(root, "app", "views", "views.go"), "package views\n// application-owned views package\n")
	if _, err := generateView(root, "welcome"); err == nil {
		t.Fatal("expected the views package collision to be rejected")
	}
	if _, err := os.Stat(filepath.Join(root, "app", "views", "pages", "welcome.gohtml")); !os.IsNotExist(err) {
		t.Fatalf("view was created despite the collision: %v", err)
	}
}

func TestGenerateViewNeedsTheLayoutOfTheGeneratedViewsPackage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
	writeFile(t, filepath.Join(root, "app", "views", "views.go"), "package views\n//go:embed */*.gohtml\nfunc New() {}\n")
	if _, err := generateView(root, "welcome"); err == nil || !strings.Contains(err.Error(), "layouts") {
		t.Fatalf("err = %v, want a complaint about the missing layout", err)
	}
}

func TestGenerateJobCreatesCancellationAwareStubAndTests(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
	var preview bytes.Buffer
	if err := runGenerateAt([]string{"job", "cleanup_old_sessions", "--dry-run"}, &preview, root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"app/jobs/cleanup_old_sessions.go", "app/jobs/cleanup_old_sessions_test.go"} {
		if !strings.Contains(preview.String(), "would create") || !strings.Contains(preview.String(), path) {
			t.Errorf("dry-run output missing %s: %s", path, preview.String())
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Errorf("dry-run created %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "app")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created directories: %v", err)
	}

	var output bytes.Buffer
	if err := runGenerateAt([]string{"job", "cleanup_old_sessions"}, &output, root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "register it in bootstrap.JobsConfigure") ||
		!strings.Contains(output.String(), "not retried automatically") {
		t.Fatalf("job guidance missing from output: %s", output.String())
	}
	jobPath := filepath.Join(root, "app", "jobs", "cleanup_old_sessions.go")
	jobSource, err := os.ReadFile(jobPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"RunCleanupOldSessions",
		"ErrCleanupOldSessionsNotImplemented",
		"ctx.Err()",
		"Every replica runs this job",
		"PostgreSQL advisory lock",
	} {
		if !strings.Contains(string(jobSource), want) {
			t.Errorf("generated job is missing %q", want)
		}
	}
	testSource, err := os.ReadFile(filepath.Join(root, "app", "jobs", "cleanup_old_sessions_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(testSource), "TestRunCleanupOldSessionsHonorsCancellation") {
		t.Fatalf("generated test does not cover cancellation: %s", testSource)
	}
	if err := runGenerateAt([]string{"job", "cleanup_old_sessions"}, &output, root); err == nil {
		t.Fatal("existing job was overwritten")
	}
}

func TestGenerateJobValidatesNamesAndApplicationRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{
		"", "Cleanup", "../escape", "x-y", "cleanup__daily", "cleanup_", "cleanup_2fa", strings.Repeat("a", 65),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := planJob(root, name); err == nil {
				t.Fatalf("accepted invalid job name %q", name)
			}
		})
	}
	if _, err := planJob(root, "cleanup"); err == nil {
		t.Fatal("generated a job outside an application")
	}
}

func TestGenerateJobRejectsSymlinkedDirectoryAndPartialCollision(t *testing.T) {
	t.Parallel()
	t.Run("symlinked directory", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		outside := t.TempDir()
		writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
		if err := os.Mkdir(filepath.Join(root, "app"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "app", "jobs")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := planJob(root, "cleanup"); err == nil {
			t.Fatal("accepted a symlinked job directory")
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("symlink target was modified: entries=%v err=%v", entries, err)
		}
	})
	t.Run("test file collision", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
		testPath := filepath.Join(root, "app", "jobs", "cleanup_test.go")
		writeFile(t, testPath, "package jobs\n")
		if _, err := planJob(root, "cleanup"); err == nil {
			t.Fatal("accepted a collision with an existing generated test")
		}
		if _, err := os.Stat(filepath.Join(root, "app", "jobs", "cleanup.go")); !os.IsNotExist(err) {
			t.Fatalf("generator left a partial file: %v", err)
		}
	})
}

func TestRunGenerateViewReportsCreatedFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
	var output bytes.Buffer
	if err := runGenerateAt([]string{"view", "landing_page"}, &output, root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"app/views/pages/landing_page.gohtml", "app/views/views.go", "app/views/layouts/application.gohtml"} {
		if !strings.Contains(output.String(), path) {
			t.Errorf("output does not report %s: %s", path, output.String())
		}
	}
}

func TestRunGenerateDryRunViewDoesNotWriteFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
	var output bytes.Buffer
	if err := runGenerateAt([]string{"view", "welcome", "--dry-run"}, &output, root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"app/views/pages/welcome.gohtml", "app/views/views.go"} {
		if !strings.Contains(output.String(), "would create") || !strings.Contains(output.String(), path) {
			t.Errorf("dry-run output missing planned path %s: %s", path, output.String())
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Errorf("dry-run created %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "app")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created directories: %v", err)
	}
}

func TestRunGenerateDryRunMigrationDoesNotWriteFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "db", "migrations")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	if err := runGenerateAtTime([]string{"migration", "create_widgets", "--dry-run"}, &output, root, now); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"db/migrations/20261004120000_create_widgets.up.sql",
		"db/migrations/20261004120000_create_widgets.down.sql",
	} {
		if !strings.Contains(output.String(), "would create") || !strings.Contains(output.String(), path) {
			t.Errorf("dry-run output missing planned path %s: %s", path, output.String())
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Errorf("dry-run created %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("dry-run changed migration directory: entries=%v err=%v", entries, err)
	}
}

func TestRunGenerateDryRunRejectsSymlinkedViewDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
	if err := os.Mkdir(filepath.Join(root, "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "app", "views")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var output bytes.Buffer
	if err := runGenerateAt([]string{"view", "welcome", "--dry-run"}, &output, root); err == nil {
		t.Fatal("dry-run accepted a symlinked views directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink target was modified: entries=%v err=%v", entries, err)
	}
}

func TestRunGenerateRejectsUnknownDryRunPlacement(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--dry-run", "view", "welcome"},
		{"view", "welcome", "--force"},
		{"view", "welcome", "--dry-run", "extra"},
	} {
		var output bytes.Buffer
		if err := runGenerateAt(args, &output, t.TempDir()); err == nil {
			t.Errorf("accepted args %v", args)
		}
	}
}

func TestApplyGeneratedFilesRollsBackPartialCreate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "db", "migrations", "conflict.down.sql"), "existing\n")
	err := applyGeneratedFiles(root, []string{"db/migrations"}, []generatedFile{
		{path: filepath.Join("db", "migrations", "conflict.up.sql"), content: "up\n"},
		{path: filepath.Join("db", "migrations", "conflict.down.sql"), content: "down\n"},
	})
	if err == nil {
		t.Fatal("expected exclusive create to reject existing down migration")
	}
	if _, err := os.Lstat(filepath.Join(root, "db", "migrations", "conflict.up.sql")); !os.IsNotExist(err) {
		t.Fatalf("partial up migration was not removed: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "db", "migrations", "conflict.down.sql"))
	if err != nil || string(content) != "existing\n" {
		t.Fatalf("existing file changed: content=%q err=%v", content, err)
	}
}

// A failed write removes the files and the directories this call created, and
// leaves what was already there.
func TestApplyGeneratedFilesRollsBackDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "kept", "existing.txt"), "mine")
	before := snapshotTree(t, root)
	err := applyGeneratedFiles(root, []string{"new/deep/dir", "kept"}, []generatedFile{
		{path: filepath.Join("new", "deep", "dir", "one.txt"), content: "1"},
		{path: filepath.Join("kept", "existing.txt"), content: "overwrite attempt"},
	})
	if err == nil {
		t.Fatal("expected the collision to fail the write")
	}
	if after := snapshotTree(t, root); strings.Join(after, "|") != strings.Join(before, "|") {
		t.Fatalf("rollback left debris:\nbefore %v\nafter  %v", before, after)
	}
	if body := readFile(t, filepath.Join(root, "kept", "existing.txt")); body != "mine" {
		t.Fatalf("existing file changed: %q", body)
	}
}
