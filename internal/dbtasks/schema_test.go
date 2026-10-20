package dbtasks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizePostgresDumpRemovesRunToRunNoise(t *testing.T) {
	t.Parallel()
	raw := `--
-- PostgreSQL database dump
--

\restrict aZ9xQ1randomtoken

-- Dumped from database version 18.6 (Ubuntu 18.6-1.pgdg24.04+2)
-- Dumped by pg_dump version 16.15 (Ubuntu 16.15-0ubuntu0.24.04.1)

SET statement_timeout = 0;

CREATE TABLE public.items (id uuid NOT NULL);

\unrestrict aZ9xQ1randomtoken
`
	got := string(normalizePostgresDump([]byte(raw)))
	for _, noise := range []string{"restrict", "Dumped from", "Dumped by", "aZ9xQ1"} {
		if strings.Contains(got, noise) {
			t.Errorf("noise %q survived:\n%s", noise, got)
		}
	}
	if !strings.Contains(got, "CREATE TABLE public.items") || !strings.Contains(got, "SET statement_timeout = 0;") {
		t.Errorf("schema lost:\n%s", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("blank-line runs not collapsed:\n%q", got)
	}
	if again := string(normalizePostgresDump([]byte(got))); again != got {
		t.Error("normalizing must be idempotent")
	}
}

func TestNormalizeMySQLDumpDropsAutoIncrementCounters(t *testing.T) {
	t.Parallel()
	got := string(normalizeMySQLDump([]byte("CREATE TABLE `t` (\n  `id` int\n) ENGINE=InnoDB AUTO_INCREMENT=42 DEFAULT CHARSET=utf8mb4;\n")))
	if strings.Contains(got, "AUTO_INCREMENT") {
		t.Fatal(got)
	}
}

func TestMigrationInsertsAreDeterministicAndSafe(t *testing.T) {
	t.Parallel()
	sum := strings.Repeat("ab", 32)
	block, err := migrationInserts([]appliedMigration{{20260101000000, "create_items", sum}, {20260102000000, "add_price", sum}}, migrationsTable)
	if err != nil {
		t.Fatal(err)
	}
	want := "INSERT INTO forge_schema_migrations (version, name, checksum, applied_at) VALUES (20260101000000, 'create_items', '" + sum + "', 0);\n"
	if !strings.Contains(block, want) || strings.Count(block, "INSERT") != 2 {
		t.Fatalf("block:\n%s", block)
	}
	for _, bad := range []appliedMigration{{1, "x'; DROP TABLE y; --", sum}, {1, "ok", "not-a-checksum"}} {
		if _, err := migrationInserts([]appliedMigration{bad}, migrationsTable); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if block, err := migrationInserts(nil, migrationsTable); block != "" || err != nil {
		t.Errorf("no migrations must give no block, got %q, %v", block, err)
	}
}

// fakeSQLite is a stand-in sqlite3 that answers the three invocations the
// dump makes, so the SQLite path is tested without the vendor tool. It
// records its arguments next to itself.
func fakeSQLite(t *testing.T) (lookPath func(string) (string, error), argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + argsFile + `'
case "$*" in
  *.schema) printf 'CREATE TABLE items (id TEXT PRIMARY KEY) STRICT;\nCREATE TABLE forge_schema_migrations (version INTEGER);\n' ;;
  *"SELECT name FROM sqlite_master"*) printf 'forge_schema_migrations\n' ;;
  *"SELECT version"*) printf '20260102000000|add_price|` + strings.Repeat("cd", 32) + `\n20260101000000|create_items|` + strings.Repeat("ab", 32) + `\n' ;;
esac
`
	path := filepath.Join(dir, "sqlite3")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { // #nosec G306 -- test script must be executable
		t.Fatal(err)
	}
	return func(name string) (string, error) {
		if name != "sqlite3" {
			return "", errors.New("unexpected tool " + name)
		}
		return path, nil
	}, argsFile
}

func TestSQLiteDumpWithTheVendorTool(t *testing.T) {
	t.Parallel()
	target := consoleTarget(t, "sqlite:storage/dev.db", nil)
	lookPath, argsFile := fakeSQLite(t)
	target.SetLookPath(lookPath)

	first, err := target.DumpSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := target.DumpSchema(context.Background())
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("dump is not deterministic: %v", err)
	}
	text := string(first)
	if !strings.HasPrefix(text, "-- Forge database schema") || !strings.Contains(text, "CREATE TABLE items") {
		t.Fatalf("dump:\n%s", text)
	}
	if !strings.Contains(text, "VALUES (20260101000000, 'create_items'") || !strings.Contains(text, "VALUES (20260102000000, 'add_price'") {
		t.Fatalf("applied migrations missing:\n%s", text)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "-readonly "+target.Name) {
		t.Fatalf("the dump must open the database read-only: %s", args)
	}
}

func TestDumpExplainsAMissingTool(t *testing.T) {
	t.Parallel()
	target := consoleTarget(t, "sqlite:storage/dev.db", nil)
	target.SetLookPath(func(string) (string, error) { return "", exec.ErrNotFound })
	if _, err := target.DumpSchema(context.Background()); err == nil || !strings.Contains(err.Error(), "sqlite3 was not found on PATH") {
		t.Fatalf("err = %v", err)
	}
}
