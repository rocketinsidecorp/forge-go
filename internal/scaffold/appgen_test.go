package scaffold_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

func TestAppFileRoundTripAndChecksum(t *testing.T) {
	t.Parallel()
	for _, features := range []scaffold.AppFeatures{
		{Module: "example.com/shop", Adapter: "sqlite"},
		{Module: "example.com/shop", Adapter: "sqlite", Identity: true},
		{Module: "example.com/shop", Adapter: "mysql", Jobs: true, Identity: true, Organizations: true},
		{Module: "example.com/shop", Adapter: "postgres", Jobs: true},
	} {
		source, err := scaffold.RenderApp(features)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := scaffold.ParseApp(source)
		if err != nil {
			t.Fatalf("%+v: %v\n%s", features, err, source)
		}
		sets, err := scaffold.RenderSets(features)
		if err != nil {
			t.Fatal(err)
		}
		parsedSets, err := scaffold.ParseSets(sets)
		if err != nil {
			t.Fatalf("%+v: %v\n%s", features, err, sets)
		}
		if parsed.Identity != features.Identity || parsed.Organizations != features.Organizations || parsedSets.Jobs != features.Jobs || parsedSets.Identity != features.Identity {
			t.Fatalf("%+v parsed as %+v and %+v", features, parsed, parsedSets)
		}
		if _, err := scaffold.ParseSets(append(sets, "// a note\n"...)); !errors.Is(err, scaffold.ErrSetsEdited) {
			t.Fatalf("an edited migration_sets_gen.go parsed: %v", err)
		}
		if strings.Contains(string(source), "SetupCommand") != features.Identity {
			t.Fatalf("%+v: setup command = %v", features, !features.Identity)
		}
		edited := strings.Replace(string(source), "const Organizations = false", "const Organizations = true", 1)
		edited = strings.Replace(edited, "const Organizations = true\n", "const Organizations = false\n", 1)
		edited += "\n// a note\n"
		if _, err := scaffold.ParseApp([]byte(edited)); !errors.Is(err, scaffold.ErrAppEdited) {
			t.Fatalf("an edited app_gen.go parsed: %v", err)
		}
	}
	for _, bad := range []scaffold.AppFeatures{{Adapter: ""}, {Adapter: "oracle"}, {Adapter: "sqlite", Organizations: true}} {
		if _, err := scaffold.RenderApp(bad); err == nil {
			t.Fatalf("%+v rendered", bad)
		}
	}
	for _, broken := range []string{"package bootstrap\n", "x\n// forge:checksum abc", "x\n// forge:checksum abc\n"} {
		if _, err := scaffold.ParseApp([]byte(broken)); !errors.Is(err, scaffold.ErrAppEdited) {
			t.Fatalf("%q parsed: %v", broken, err)
		}
	}
}

func TestAuthAndTenantsOptions(t *testing.T) {
	t.Parallel()
	opts, err := scaffold.Normalize(scaffold.Options{Name: "saas", Database: "postgresql", Tenants: true})
	if err != nil || !opts.Auth || !opts.Tenants {
		t.Fatalf("--tenants: %+v %v", opts, err)
	}
	for _, bad := range []scaffold.Options{
		{Name: "x", Auth: true},
		{Name: "x", Tenants: true},
		{Name: "x", Database: "sqlite", API: true, Auth: true},
		{Name: "x", Database: "sqlite", API: true, Tenants: true},
	} {
		if _, err := scaffold.Normalize(bad); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
	for opts, want := range map[scaffold.Options]string{
		{Name: "site", Database: "sqlite"}:                                "forge new site",
		{Name: "shop", Database: "sqlite", Auth: true}:                    "forge new shop --auth",
		{Name: "shop", Database: "mysql", Auth: true}:                     "forge new shop -d mysql --auth",
		{Name: "saas", Database: "postgresql", Tenants: true, Auth: true}: "forge new saas --tenants",
		{Name: "saas", Database: "sqlite", Tenants: true, Auth: true}:     "forge new saas -d sqlite3 --tenants",
		{Name: "api", Database: "postgresql", API: true}:                  "forge new api -d postgresql --api",
		{Name: "edge"}: "forge new edge -d none",
		{Name: "shop", Database: "sqlite", Auth: true, APIAuth: true, Locale: "es"}: "forge new shop --api-auth --locale es",
	} {
		if got := scaffold.NewCommand(opts); got != want {
			t.Errorf("%+v: %q, want %q", opts, got, want)
		}
	}
}

// TestTenantsApplication: forge new --tenants has organizations: app_gen.go
// says so, setup asks for --org, and the README's quickstart scaffolds
// --org-scoped.
func TestTenantsApplication(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "saas")
	if _, err := scaffold.Generate(scaffold.Options{Name: "saas", Module: "example.com/saas", Dir: dir, Database: "postgresql", Tenants: true}); err != nil {
		t.Fatal(err)
	}
	assertContains(t, dir, map[string][]string{
		"app/bootstrap/app_gen.go":    {"const Organizations = true"},
		"app/bootstrap/routes_gen.go": {"// forge:identity repository"},
		"README.md":                   {"forge new saas --tenants\n", "forge setup --org acme --email you@example.com\n", "--org-scoped\n", "--data-urlencode tenant=acme"},
	})
}
