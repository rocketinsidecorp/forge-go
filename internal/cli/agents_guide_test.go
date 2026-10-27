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
		name     string
		options  scaffold.Options
		database bool // migrations, tenant scoping
		browser  bool // views and CSRF
		identity bool // sign-in pages (PostgreSQL, full-stack)
	}{
		{"postgresql", scaffold.Options{Database: "postgresql"}, true, true, true},
		{"sqlite", scaffold.Options{Database: "sqlite"}, true, true, false},
		{"mysql", scaffold.Options{Database: "mysql"}, true, true, false},
		{"api postgresql", scaffold.Options{Database: "postgresql", API: true}, true, false, false},
		{"api sqlite", scaffold.Options{Database: "sqlite", API: true}, true, false, false},
		{"no database", scaffold.Options{}, false, true, false},
		{"api no database", scaffold.Options{API: true}, false, false, false},
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
				"migrations":         {"forge migrate", c.database},
				"tenant scoping":     {"tenancy.Require", c.database},
				"model generator":    {"scaffold|resource|model", c.database && c.browser},
				"CSRF":               {"csrfField", c.browser},
				"view generator":     {"|view NAME", c.browser},
				"sign-in":            {"Sign-in, sign-out", c.identity},
				"row-level security": {"app_runtime", c.identity},
			} {
				if got := strings.Contains(guide, want.text); got != want.present {
					t.Errorf("AGENTS.md mentions %s (%q) = %v, want %v", feature, want.text, got, want.present)
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
