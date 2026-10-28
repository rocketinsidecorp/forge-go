package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/migrate"
)

var commandMigrations = fstest.MapFS{
	"20260101000000_first.up.sql":    {Data: []byte("CREATE TABLE first (id INTEGER);")},
	"20260101000000_first.down.sql":  {Data: []byte("DROP TABLE first;")},
	"20260102000000_second.up.sql":   {Data: []byte("CREATE TABLE second (id INTEGER);")},
	"20260102000000_second.down.sql": {Data: []byte("DROP TABLE second;")},
}

// commandRig runs the migrate and rollback commands over one SQLite file.
type commandRig struct {
	commands map[string]forge.Command
	env      forge.CommandEnv
	stdout   *bytes.Buffer
	opened   int
}

func newCommandRig(t *testing.T, sets ...fs.FS) *commandRig {
	t.Helper()
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "commands.db"))
	rig := &commandRig{commands: map[string]forge.Command{}, stdout: &bytes.Buffer{}}
	open := func(_ context.Context, _ config.Database, migrations []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		rig.opened++
		// Every command gets its own connection to the same database file,
		// like separate processes, and closes it when it is done.
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, err
		}
		return migrate.New(db, &scriptedDialect{}, migrations, logger)
	}
	for _, command := range migrate.CommandsFromSets(sets, open) {
		rig.commands[command.Name] = command
	}
	cfg := config.Default()
	cfg.Database.URL = config.NewSecret("sqlite:/unused/for/this/test.db")
	rig.env = forge.CommandEnv{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Stdout: rig.stdout, Stderr: io.Discard}
	return rig
}

func (r *commandRig) run(name string, args ...string) (string, error) {
	r.stdout.Reset()
	err := r.commands[name].Run(context.Background(), r.env, args)
	return r.stdout.String(), err
}

func TestMigrateAndRollbackCommandsReportWhatTheyDo(t *testing.T) {
	t.Parallel()
	rig := newCommandRig(t, commandMigrations)
	out, err := rig.run("migrate")
	if err != nil || !strings.Contains(out, "applied  20260101000000_first") || !strings.Contains(out, "applied  20260102000000_second") {
		t.Fatalf("migrate = %q, %v", out, err)
	}
	if out, err := rig.run("migrate"); err != nil || !strings.Contains(out, "database is up to date") {
		t.Errorf("migrate again = %q, %v", out, err)
	}
	out, err = rig.run("migrate", "status")
	if err != nil || strings.Count(out, "applied") != 2 || !strings.Contains(out, "20260102000000_second") {
		t.Errorf("status = %q, %v", out, err)
	}
	// Rolling back reverts the most recent migration and says so.
	if out, err := rig.run("rollback"); err != nil || !strings.Contains(out, "reverted 20260102000000_second") || strings.Contains(out, "first") {
		t.Errorf("rollback = %q, %v", out, err)
	}
	if out, err := rig.run("rollback", "-steps", "5"); err != nil || !strings.Contains(out, "reverted 20260101000000_first") {
		t.Errorf("rollback -steps 5 = %q, %v", out, err)
	}
	out, err = rig.run("migrate", "status")
	if err != nil || strings.Count(out, "pending") != 2 {
		t.Errorf("status after reverting everything = %q, %v", out, err)
	}
}

func TestMigrateCommandsRefuseBadArgumentsAndAnUnusableSetup(t *testing.T) {
	t.Parallel()
	rig := newCommandRig(t, commandMigrations)
	for name, call := range map[string]func() error{
		"migrate with an operand":       func() error { _, err := rig.run("migrate", "up"); return err },
		"migrate status with extras":    func() error { _, err := rig.run("migrate", "status", "now"); return err },
		"rollback with an operand":      func() error { _, err := rig.run("rollback", "one"); return err },
		"rollback without any steps":    func() error { _, err := rig.run("rollback", "-steps", "0"); return err },
		"rollback with a negative one":  func() error { _, err := rig.run("rollback", "-steps", "-2"); return err },
		"rollback with an unknown flag": func() error { _, err := rig.run("rollback", "-nope"); return err },
	} {
		if err := call(); !errors.Is(err, forge.ErrUsage) {
			t.Errorf("%s: err = %v, want a usage error", name, err)
		}
	}
	if rig.opened != 0 {
		t.Errorf("a database was opened for a command line that was refused (%d times)", rig.opened)
	}

	// No database configured.
	rig.env.Config.Database.URL = config.Secret{}
	if _, err := rig.run("migrate"); err == nil || !strings.Contains(err.Error(), "FORGE_DATABASE_URL") {
		t.Errorf("without a database: %v", err)
	}
	// A database that cannot be opened.
	rig.env.Config.Database.URL = config.NewSecret("sqlite:/unused/for/this/test.db")
	failing := migrate.Commands(commandMigrations, func(context.Context, config.Database, []migrate.Migration, *slog.Logger) (*migrate.Migrator, error) {
		return nil, errors.New("connection refused")
	})
	if err := failing[0].Run(context.Background(), rig.env, nil); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("an unreachable database: %v", err)
	}
	// A broken migration set stops before any connection.
	broken := newCommandRig(t, fstest.MapFS{"not-a-migration.sql": {Data: []byte("SELECT 1;")}})
	if _, err := broken.run("migrate"); err == nil || broken.opened != 0 {
		t.Errorf("a malformed migration set: %v (opened %d)", err, broken.opened)
	}
}

func TestLoadMigrationsReportsWhatItCannotRead(t *testing.T) {
	t.Parallel()
	if _, err := migrate.LoadMigrationSets(); err != nil {
		t.Errorf("no sets: %v", err)
	}
	if _, err := migrate.LoadMigrationSets(commandMigrations, nil); err == nil || !strings.Contains(err.Error(), "migration set 2 is nil") {
		t.Errorf("a nil set: %v", err)
	}
	if _, err := migrate.LoadMigrationSets(fstest.MapFS{"x.sql": {Data: []byte("SELECT 1;")}}); err == nil || !strings.Contains(err.Error(), "migration set 1") {
		t.Errorf("a malformed set is not named: %v", err)
	}
	if _, err := migrate.LoadMigrations(unreadable{}); err == nil || !strings.Contains(err.Error(), "read migrations") {
		t.Errorf("an unreadable filesystem: %v", err)
	}
	if _, err := migrate.LoadMigrations(unreadableFile{fstest.MapFS{"20260101000000_a.up.sql": {Data: []byte("SELECT 1;")}}}); err == nil || !strings.Contains(err.Error(), "read migration 20260101000000_a.up.sql") {
		t.Errorf("an unreadable file: %v", err)
	}
	// A version outside the integer range is not a version.
	if _, err := migrate.LoadMigrations(fstest.MapFS{"99999999999999999999_a.up.sql": {Data: []byte("SELECT 1;")}}); err == nil {
		t.Error("an enormous version was accepted")
	}
}

type unreadable struct{}

func (unreadable) Open(string) (fs.File, error) { return nil, errors.New("disk on fire") }

// unreadableFile lists its files but cannot read them.
type unreadableFile struct{ fstest.MapFS }

func (u unreadableFile) ReadFile(string) ([]byte, error) { return nil, errors.New("disk on fire") }

func (u unreadableFile) Open(name string) (fs.File, error) {
	if name == "." {
		return u.MapFS.Open(name)
	}
	return nil, errors.New("disk on fire")
}
