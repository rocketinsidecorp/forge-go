package dbtasks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/rocketinsidecorp/forge-go/config"
)

const migrationsTable = "forge_schema_migrations"

// SchemaFile is where `forge db schema:dump` writes, relative to the
// application root.
const SchemaFile = "db/schema.sql"

const schemaHeader = "-- Forge database schema, written by `forge db schema:dump`.\n" +
	"-- Do not edit by hand: change db/migrations and dump again.\n" +
	"-- Load it into an empty database with `forge db schema:load`.\n"

type appliedMigration struct {
	version  int64
	name     string
	checksum string
}

// DumpSchema returns the schema of the database as deterministic SQL: the
// vendor tool's schema-only output, minus anything that varies from run to
// run (versions, dates, random tokens), followed by INSERT statements that
// record the applied migrations so a loaded database knows its history. It
// needs the vendor tool (pg_dump, mysqldump or sqlite3) on PATH.
func (t *Target) DumpSchema(ctx context.Context) ([]byte, error) {
	var (
		dump    []byte
		applied []appliedMigration
		table   = migrationsTable
		err     error
	)
	switch t.Adapter {
	case config.AdapterPostgres:
		if dump, err = t.pgDump(ctx); err != nil {
			return nil, err
		}
		dump = normalizePostgresDump(dump)
		applied, table, err = t.pgAppliedMigrations(ctx)
	case config.AdapterMySQL:
		if dump, err = t.mysqlDump(ctx); err != nil {
			return nil, err
		}
		dump = normalizeMySQLDump(dump)
		applied, err = t.mysqlAppliedMigrations(ctx)
	default:
		if dump, err = t.sqliteDump(ctx); err != nil {
			return nil, err
		}
		applied, err = t.sqliteAppliedMigrations(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	var out bytes.Buffer
	out.WriteString(schemaHeader)
	out.WriteByte('\n')
	out.Write(bytes.TrimRight(dump, "\n"))
	out.WriteByte('\n')
	if block, err := migrationInserts(applied, table); err != nil {
		return nil, err
	} else if block != "" {
		out.WriteString("\n" + block)
	}
	return out.Bytes(), nil
}

// migrationInserts renders the applied migrations. applied_at is 0 on
// purpose: a timestamp would make every dump differ. The names and
// checksums come from files the migration engine validated (lowercase
// letters, digits, underscores, hex), and are checked again here.
func migrationInserts(applied []appliedMigration, table string) (string, error) {
	if len(applied) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("--\n-- Applied migrations.\n--\n\n")
	for _, m := range applied {
		if !safeMigrationName.MatchString(m.name) || !safeChecksum.MatchString(m.checksum) {
			return "", fmt.Errorf("migration %d has a name or checksum Forge will not write into a schema file", m.version)
		}
		fmt.Fprintf(&b, "INSERT INTO %s (version, name, checksum, applied_at) VALUES (%d, '%s', '%s', 0);\n", table, m.version, m.name, m.checksum)
	}
	return b.String(), nil
}

var (
	safeMigrationName = regexp.MustCompile(`^[a-z0-9_]+$`)
	safeChecksum      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// postgresNoise matches the lines of pg_dump output that vary between runs
// and tool versions: the version comments and the \restrict token (random
// per run since the 2025 security releases).
var postgresNoise = regexp.MustCompile(`^(-- Dumped (from|by) |\\restrict |\\unrestrict )`)

func normalizePostgresDump(raw []byte) []byte {
	var out bytes.Buffer
	blank := 0
	for line := range strings.Lines(string(raw)) {
		trimmed := strings.TrimRight(line, "\r\n")
		if postgresNoise.MatchString(trimmed) {
			continue
		}
		// Dropping a comment can leave a run of blank lines; keep at most one.
		if trimmed == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out.WriteString(trimmed + "\n")
	}
	return bytes.TrimLeft(out.Bytes(), "\n")
}

var mysqlAutoIncrement = regexp.MustCompile(` AUTO_INCREMENT=\d+`)

func normalizeMySQLDump(raw []byte) []byte {
	return mysqlAutoIncrement.ReplaceAll(raw, nil)
}

func (t *Target) pgDump(ctx context.Context) ([]byte, error) {
	out, err := t.run(ctx, tool{
		name: "pg_dump",
		args: []string{"--schema-only", "--no-owner", "--no-privileges", "--no-tablespaces"},
		env:  t.pgEnvironment(environ()),
		dir:  t.root,
	})
	if err != nil && strings.Contains(err.Error(), "server version mismatch") {
		return nil, fmt.Errorf("%w\nput a pg_dump of the server's major version or newer first on PATH", err)
	}
	return out, err
}

func (t *Target) mysqlDump(ctx context.Context) ([]byte, error) {
	file, cleanup, err := t.mysqlOptions("mysqldump", t.Name)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return t.run(ctx, tool{
		name: "mysqldump",
		// The option file must come first. --skip-add-drop-table keeps the
		// schema file from dropping tables when it is loaded.
		args: []string{mysqlDefaults(file), "--no-data", "--skip-comments", "--skip-dump-date", "--skip-add-drop-table", "--no-tablespaces", "--routines", "--triggers", t.Name},
		env:  mysqlEnvironment(environ(), file),
		dir:  t.root,
	})
}

func (t *Target) sqliteDump(ctx context.Context) ([]byte, error) {
	return t.run(ctx, tool{name: "sqlite3", args: []string{"-readonly", t.Name, ".schema"}, env: environ(), dir: t.root})
}

func (t *Target) sqliteAppliedMigrations(ctx context.Context) ([]appliedMigration, error) {
	// A column list that cannot be confused with the data: fields are
	// validated afterwards.
	query := "SELECT version || '|' || name || '|' || checksum FROM " + migrationsTable + " ORDER BY version"
	tracked, err := t.run(ctx, tool{name: "sqlite3", args: []string{"-readonly", "-batch", t.Name, "SELECT name FROM sqlite_master WHERE type = 'table' AND name = '" + migrationsTable + "'"}, env: environ(), dir: t.root})
	if err != nil || len(bytes.TrimSpace(tracked)) == 0 {
		return nil, err
	}
	out, err := t.run(ctx, tool{name: "sqlite3", args: []string{"-readonly", "-batch", t.Name, query}, env: environ(), dir: t.root})
	if err != nil {
		return nil, err
	}
	var applied []appliedMigration
	for line := range strings.Lines(string(out)) {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 3 {
			return nil, errors.New("unexpected migration row from sqlite3")
		}
		var m appliedMigration
		if _, err := fmt.Sscanf(parts[0], "%d", &m.version); err != nil {
			return nil, errors.New("unexpected migration version from sqlite3")
		}
		m.name, m.checksum = parts[1], parts[2]
		applied = append(applied, m)
	}
	return applied, nil
}

// LoadSchema runs the SQL file into the (existing, normally empty) database
// with the vendor client. Tables that already exist make it fail rather
// than be replaced.
func (t *Target) LoadSchema(ctx context.Context, schemaPath string) error {
	if err := t.checkMutable(); err != nil {
		return err
	}
	exists, err := t.Exists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("the database does not exist: run forge db create first")
	}
	file, err := os.Open(schemaPath) // #nosec G304 -- the schema file path forge chose
	if err != nil {
		return fmt.Errorf("open schema file: %w", err)
	}
	defer func() { _ = file.Close() }()

	switch t.Adapter {
	case config.AdapterPostgres:
		_, err = t.run(ctx, tool{
			name: "psql",
			args: []string{"-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-f", schemaPath},
			env:  t.pgEnvironment(environ()),
			dir:  t.root,
		})
	case config.AdapterMySQL:
		var options string
		var cleanup func()
		if options, cleanup, err = t.mysqlOptions("mysql", t.Name); err != nil {
			return err
		}
		defer cleanup()
		_, err = t.run(ctx, tool{name: "mysql", args: []string{mysqlDefaults(options)}, env: mysqlEnvironment(environ(), options), dir: t.root, stdin: file})
	default:
		// SQLite DDL is transactional: one failure leaves nothing behind.
		stdin := io.MultiReader(strings.NewReader("BEGIN;\n"), file, strings.NewReader("\nCOMMIT;\n"))
		_, err = t.run(ctx, tool{name: "sqlite3", args: []string{"-bail", t.Name}, env: environ(), dir: t.root, stdin: stdin})
	}
	if err != nil {
		return fmt.Errorf("load schema: %w", err)
	}
	return nil
}
