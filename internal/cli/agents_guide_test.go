package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

var (
	agentsCommandLine = regexp.MustCompile(`(?m)^forge (\S+)`)
	agentsPath        = regexp.MustCompile("`((?:app|cmd|db|config)/[A-Za-z0-9_./-]+\\.(?:go|md))`")
)

// TestGeneratedAgentsGuideMatchesTheApplication generates every kind of
// application and checks that AGENTS.md is short and says only what that
// application has: every forge command it shows exists, every file it names
// was generated, and a feature is mentioned only when the application has it.
func TestGeneratedAgentsGuideMatchesTheApplication(t *testing.T) {
	for _, c := range []struct {
		name          string
		options       scaffold.Options
		database      bool // migrations
		browser       bool // views and CSRF
		identity      bool // sign-in pages and setup (--auth, full-stack)
		organizations bool // org-scoped tables (--tenants)
		roles         bool // the app_runtime and app_migrator roles (PostgreSQL, also API-only)
	}{
		{"default (sqlite, no auth)", scaffold.Options{Database: "sqlite"}, true, true, false, false, false},
		{"sqlite --auth", scaffold.Options{Database: "sqlite", Auth: true}, true, true, true, false, false},
		{"mysql --auth", scaffold.Options{Database: "mysql", Auth: true}, true, true, true, false, false},
		{"mysql", scaffold.Options{Database: "mysql"}, true, true, false, false, false},
		{"postgresql", scaffold.Options{Database: "postgresql"}, true, true, false, false, true},
		{"postgresql --auth", scaffold.Options{Database: "postgresql", Auth: true}, true, true, true, false, true},
		{"--tenants (postgresql)", scaffold.Options{Database: "postgresql", Tenants: true}, true, true, true, true, true},
		{"sqlite --tenants", scaffold.Options{Database: "sqlite", Tenants: true}, true, true, true, true, false},
		{"--api-auth", scaffold.Options{Database: "sqlite", APIAuth: true}, true, true, true, false, false},
		{"sqlite --js", scaffold.Options{Database: "sqlite", JS: true}, true, true, false, false, false},
		{"--auth --js", scaffold.Options{Database: "sqlite", Auth: true, JS: true}, true, true, true, false, false},
		{"api postgresql", scaffold.Options{Database: "postgresql", API: true}, true, false, false, false, true},
		{"api sqlite", scaffold.Options{Database: "sqlite", API: true}, true, false, false, false, false},
		{"no database", scaffold.Options{}, false, true, false, false, false},
		{"api no database", scaffold.Options{API: true}, false, false, false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			options := c.options
			options.Name = "shop"
			options.Dir = filepath.Join(t.TempDir(), "shop")
			if _, err := scaffold.Generate(options); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(options.Dir, "AGENTS.md"))
			if err != nil {
				t.Fatal(err)
			}
			guide := string(raw)
			if lines := strings.Count(guide, "\n"); lines > 120 {
				t.Errorf("AGENTS.md has %d lines; keep it under 120", lines)
			}

			generateUsage := commandDetailNamed("generate").Usage
			for _, match := range agentsCommandLine.FindAllStringSubmatch(guide, -1) {
				if match[1] != "new" && commandDetailNamed(match[1]) == nil {
					t.Errorf("AGENTS.md shows `forge %s`, which is not a command", match[1])
				}
			}
			for _, line := range strings.Split(guide, "\n") {
				if kinds, ok := strings.CutPrefix(line, "forge generate "); ok {
					kinds, _, _ = strings.Cut(kinds, " ")
					for kind := range strings.SplitSeq(kinds, "|") {
						if !strings.Contains(generateUsage, kind) {
							t.Errorf("AGENTS.md shows `forge generate %s`, which `forge help generate` does not list", kind)
						}
					}
				}
			}
			for _, match := range agentsPath.FindAllStringSubmatch(guide, -1) {
				if _, err := os.Stat(filepath.Join(options.Dir, filepath.FromSlash(match[1]))); err != nil {
					t.Errorf("AGENTS.md names %s, which the application does not have", match[1])
				}
			}

			for feature, want := range map[string]struct {
				text    string
				present bool
			}{
				"migrations":       {"forge migrate", c.database},
				"org scoping":      {"tenancy.Require", c.organizations},
				"org scope flag":   {"`--org-scoped` (rows belong", c.organizations},
				"tenant naming":    {"tenant_id", false},
				"old scope flag":   {"--tenant-scoped", false},
				"plain tables":     {"Generated tables are plain tables", c.database && !c.identity},
				"one org":          {"This application has one organization", c.identity && !c.organizations},
				"setup":            {"forge setup", c.identity},
				"setup --org":      {"forge setup --org", c.organizations},
				"generate tenancy": {"forge generate\ntenancy", c.identity && !c.organizations},
				"model generator":  {"scaffold|resource|model", c.database && c.browser},
				"CSRF":             {"csrfField", c.browser},
				"view generator":   {"|view NAME", c.browser},
				"sign-in":          {"Sign-in, sign-out", c.identity},
				"runtime roles":    {"app_runtime", c.roles},
				"migrator role":    {"app_migrator", c.roles},
				"test files":       {"`_test.go` files too", true},
				"TypeScript":       {"never edit `dist` by hand", c.browser && !c.options.JS},
				"tsc":              {"tsc --noEmit", c.browser && !c.options.JS},
				"plain JS":         {"plain JavaScript", c.browser && c.options.JS},
			} {
				if got := strings.Contains(guide, want.text); got != want.present {
					t.Errorf("AGENTS.md mentions %s (%q) = %v, want %v", feature, want.text, got, want.present)
				}
			}

			// An API-only application has no app/views: no document names it,
			// and the other applications name it in each of them.
			readme, err := os.ReadFile(filepath.Join(options.Dir, "README.md"))
			if err != nil {
				t.Fatal(err)
			}
			_, architectureSection, _ := strings.Cut(string(readme), "\n## Architecture\n")
			architectureSection, _, _ = strings.Cut(architectureSection, "\n## ")
			if architectureSection == "" {
				t.Fatal("README.md has no Architecture section")
			}
			testFile, err := os.ReadFile(filepath.Join(options.Dir, "app", "architecture_test.go"))
			if err != nil {
				t.Fatal(err)
			}
			for document, text := range map[string]string{
				"AGENTS.md": guide, "the README Architecture section": architectureSection, "app/architecture_test.go": string(testFile),
			} {
				if got := strings.Contains(text, "app/views"); got != c.browser {
					t.Errorf("%s mentions app/views = %v, want %v", document, got, c.browser)
				}
			}

			// The rings it describes are the ones the architecture test enforces.
			architecture, err := os.ReadFile(filepath.Join(options.Dir, "app", "architecture_test.go"))
			if err != nil {
				t.Fatal(err)
			}
			for _, ring := range []string{"app/domain", "app/application", "app/adapters", "app/bootstrap", "extraAllowedImports", "TestArchitecture"} {
				if !strings.Contains(guide, ring) || !strings.Contains(string(architecture), ring) && ring != "TestArchitecture" {
					t.Errorf("AGENTS.md and app/architecture_test.go disagree about %s", ring)
				}
			}
		})
	}
}
