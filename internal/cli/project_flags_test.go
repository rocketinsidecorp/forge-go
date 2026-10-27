package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewChoosesTheDatabaseAndIdentity is decisions D31, D33 and D35: SQLite
// by default and no authentication; --auth adds Forge identity with one
// organization; --tenants adds organizations on PostgreSQL unless -d says
// otherwise; --api-auth implies --auth.
func TestNewChoosesTheDatabaseAndIdentity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		flags    []string
		url      string
		identity bool
		orgs     bool
		next     []string
	}{
		{nil, "FORGE_DATABASE_URL=sqlite:", false, false, []string{"forge migrate\n  forge dev", "welcome screen"}},
		{[]string{"--auth"}, "FORGE_DATABASE_URL=sqlite:", true, false, []string{"forge setup --email you@example.com", "/session/new"}},
		{[]string{"--tenants"}, "FORGE_DATABASE_URL=postgres://", true, true, []string{"docker compose up -d --wait postgres", "forge setup --org acme --email you@example.com"}},
		{[]string{"--tenants", "-d", "mysql"}, "FORGE_DATABASE_URL=mysql://", true, true, []string{"docker compose up -d --wait mysql"}},
		{[]string{"--api-auth", "-d", "mariadb"}, "FORGE_DATABASE_URL=mysql://", true, false, []string{"docker compose up -d --wait mariadb"}},
		{[]string{"--api"}, "FORGE_DATABASE_URL=sqlite:", false, false, []string{"curl http://127.0.0.1:8080/"}},
	} {
		dir := filepath.Join(t.TempDir(), "shop")
		var stdout, stderr bytes.Buffer
		args := append([]string{"new", "shop", "--dir", dir, "--skip-deps"}, test.flags...)
		if code := Run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: code=%d %s", test.flags, code, stderr.String())
		}
		if env := readFile(t, filepath.Join(dir, ".env.development")); !strings.Contains(env, test.url) {
			t.Errorf("%v: .env.development lacks %q", test.flags, test.url)
		}
		app := readFile(t, filepath.Join(dir, "app", "bootstrap", "app_gen.go"))
		if strings.Contains(app, "SetupCommand") != test.identity || strings.Contains(app, "const Organizations = true") != test.orgs {
			t.Errorf("%v: app_gen.go:\n%s", test.flags, app)
		}
		for _, want := range test.next {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("%v: next steps lack %q:\n%s", test.flags, want, stdout.String())
			}
		}
	}
	for _, refused := range [][]string{
		{"--auth", "--skip-database"},
		{"--auth", "-d", "none"},
		{"--api", "--auth"},
		{"--api", "--tenants"},
		{"-d", ""},
	} {
		var stdout, stderr bytes.Buffer
		args := append([]string{"new", "shop", "--dir", filepath.Join(t.TempDir(), "shop"), "--skip-deps"}, refused...)
		if code := Run(args, &stdout, &stderr); code == 0 {
			t.Errorf("%v was accepted", refused)
		}
	}
}
