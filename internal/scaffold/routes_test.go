package scaffold_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

func TestRoutesSignatureFollowsTheApplication(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		app        scaffold.RoutesApp
		parameters string
		call       string
	}{
		{scaffold.RoutesApp{Module: "m", Adapter: "postgres", Identity: true},
			"ctx context.Context, app *forge.App, database *postgres.DB, translator *i18n.Translator, repository *authpostgres.Repository, middleware *auth.Middleware",
			"generatedRoutes(ctx, app, database, translator, repository, middleware)"},
		{scaffold.RoutesApp{Module: "m", Adapter: "sqlite"}, "ctx context.Context, app *forge.App, database *sqldb.DB, translator *i18n.Translator", "generatedRoutes(ctx, app, database, translator)"},
		{scaffold.RoutesApp{Module: "m", Adapter: "postgres", API: true}, "ctx context.Context, app *forge.App, database *postgres.DB", "generatedRoutes(ctx, app, database)"},
		{scaffold.RoutesApp{Module: "m"}, "ctx context.Context, app *forge.App, translator *i18n.Translator", "generatedRoutes(ctx, app, translator)"},
		{scaffold.RoutesApp{Module: "m", API: true}, "ctx context.Context, app *forge.App", "generatedRoutes(ctx, app)"},
	} {
		if got := scaffold.RoutesParameters(test.app); got != test.parameters {
			t.Errorf("%+v: parameters %q", test.app, got)
		}
		if got := scaffold.RoutesCall(test.app); got != test.call {
			t.Errorf("%+v: call %q", test.app, got)
		}
	}
}

func TestRoutesRoundTripAndChecksum(t *testing.T) {
	t.Parallel()
	app := scaffold.RoutesApp{Module: "example.com/shop", Adapter: "sqlite"}
	blocks := []scaffold.RouteBlock{
		{Kind: "guard", Name: "public", Body: "func public(_ string, next http.Handler) (http.Handler, error) {\n\treturn next, nil\n}\n"},
		{Kind: "controller", Name: "pages", Body: "\tif err := htmlui.RegisterPagesController(app, nil, public); err != nil {\n\t\treturn err\n\t}\n"},
	}
	source, err := scaffold.RenderRoutes(app, blocks)
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, want := range []string{
		"import (\n\t\"context\"\n\t\"net/http\"\n\n\t\"example.com/shop/app/adapters/htmlui\"\n\tforge \"github.com/rocketinsidecorp/forge-go\"\n\t\"github.com/rocketinsidecorp/forge-go/i18n\"\n\t\"github.com/rocketinsidecorp/forge-go/sqldb\"\n)",
		"\n// forge:guard public\nfunc public(", "\t// forge:controller pages\n", "// forge:checksum ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered seam lacks %q:\n%s", want, text)
		}
	}
	parsed, err := scaffold.ParseRoutes(source)
	if err != nil {
		t.Fatal(err)
	}
	// Statements of generatedRoutes first, then the package-level guards.
	if len(parsed) != 2 || parsed[0] != blocks[1] || parsed[1] != blocks[0] || !parsed[1].Shared() || !parsed[1].TopLevel() || parsed[0].Shared() || parsed[0].TopLevel() {
		t.Fatalf("parsed %+v", parsed)
	}
	again, err := scaffold.RenderRoutes(app, parsed)
	if err != nil || string(again) != text {
		t.Fatalf("rendering the parsed blocks changed the seam: %v", err)
	}
	sum := strings.Index(text, "// forge:checksum ") + len("// forge:checksum ")
	for name, edited := range map[string]string{
		"a changed line":       strings.Replace(text, "return next, nil", "return nil, nil", 1),
		"no checksum":          strings.Replace(text, "// forge:checksum ", "// checksum ", 1),
		"a short checksum":     text[:sum] + "abc" + text[sum+64:],
		"a truncated file":     text[:sum+30],
		"no function":          strings.Replace(text, "func generatedRoutes(", "func otherRoutes(", 1),
		"no return":            strings.Replace(text, "\treturn nil\n}", "\treturn errors.New(\"x\")\n}", 1),
		"text before a marker": strings.Replace(text, "\t// forge:controller pages\n", "\tx := 1\n\t// forge:controller pages\n", 1),
	} {
		if _, err := scaffold.ParseRoutes([]byte(edited)); !errors.Is(err, scaffold.ErrRoutesEdited) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := scaffold.RenderRoutes(app, []scaffold.RouteBlock{{Kind: "Guard", Name: "x y"}}); err == nil {
		t.Error("an invalid block ID was rendered")
	}
	if _, err := scaffold.RenderRoutes(app, []scaffold.RouteBlock{{Kind: "guard", Name: "x", Body: "\tx := func( {\n"}}); err == nil {
		t.Error("a block that does not parse was rendered")
	}
}

// TestNewApplicationsCallTheEmptySeam: every kind of application gets the
// seam, empty and valid, and a bootstrap.go that calls it with the
// parameters it declares.
func TestNewApplicationsCallTheEmptySeam(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		options scaffold.Options
		app     scaffold.RoutesApp
	}{
		{scaffold.Options{Database: "postgresql"}, scaffold.RoutesApp{Adapter: "postgres", Identity: true}},
		{scaffold.Options{Database: "mysql"}, scaffold.RoutesApp{Adapter: "mysql"}},
		{scaffold.Options{Database: "sqlite"}, scaffold.RoutesApp{Adapter: "sqlite"}},
		{scaffold.Options{Database: "postgresql", API: true}, scaffold.RoutesApp{Adapter: "postgres", API: true}},
		{scaffold.Options{}, scaffold.RoutesApp{}},
		{scaffold.Options{API: true}, scaffold.RoutesApp{API: true}},
	} {
		dir := filepath.Join(t.TempDir(), "shop")
		test.options.Name, test.options.Module, test.options.Dir = "shop", "example.com/shop", dir
		if _, err := scaffold.Generate(test.options); err != nil {
			t.Fatal(err)
		}
		seam, err := os.ReadFile(filepath.Join(dir, scaffold.RoutesFile))
		if err != nil {
			t.Fatal(err)
		}
		if blocks, err := scaffold.ParseRoutes(seam); err != nil || len(blocks) != 0 {
			t.Errorf("%+v: the new seam has blocks %v or does not verify: %v", test.options, blocks, err)
		}
		test.app.Module = "example.com/shop"
		if !strings.Contains(string(seam), "func generatedRoutes("+scaffold.RoutesParameters(test.app)+") error {") {
			t.Errorf("%+v: the seam's signature is not %s:\n%s", test.options, scaffold.RoutesParameters(test.app), seam)
		}
		bootstrap, err := os.ReadFile(filepath.Join(dir, "app", "bootstrap", "bootstrap.go"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(bootstrap), scaffold.RoutesCall(test.app)) {
			t.Errorf("%+v: bootstrap.go does not call %s", test.options, scaffold.RoutesCall(test.app))
		}
		readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		if hasQuickstart := strings.Contains(string(readme), "## Quickstart"); hasQuickstart != (test.app.Adapter != "" && !test.app.API) {
			t.Errorf("%+v: README quickstart = %v", test.options, hasQuickstart)
		}
	}
}
