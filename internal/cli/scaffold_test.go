package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func runScaffold(root string, args ...string) (string, error) {
	var out bytes.Buffer
	err := runGenerateEnv(append([]string{"scaffold"}, args...), &out, root, nil, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	return out.String(), err
}

// pipelineSource is the part of bootstrap.go `forge new` generates that the
// scaffold looks for.
const pipelineSource = "package bootstrap\n\n// csrf.New( web.MethodOverride web.Flash locales.New( .Middleware(i18n.NegotiationOptions\n"

func scaffoldProject(t *testing.T, databaseURL string) string {
	t.Helper()
	root := resourceProject(t, databaseURL)
	writeFile(t, filepath.Join(root, "app", "bootstrap", "bootstrap.go"), pipelineSource)
	return root
}

func TestScaffoldUsageNamesTheScaffoldCommand(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{}, {"--global"}, {"post", "--global"}, {"post", "title:string"}, {"post", "title:string", "--global", "--bogus"}} {
		_, err := runScaffold(t.TempDir(), args...)
		if err == nil {
			t.Errorf("%v was accepted", args)
			continue
		}
		if strings.Contains(err.Error(), "forge generate resource") {
			t.Errorf("%v: the error names the resource command: %v", args, err)
		}
	}
	if _, err := runScaffold(t.TempDir(), "--global"); err == nil || !strings.Contains(err.Error(), scaffoldUsage) {
		t.Errorf("err = %v, want the scaffold usage", err)
	}
}

func TestScaffoldDryRunWritesNothingAndPrintsTheHTMLWiring(t *testing.T) {
	t.Parallel()
	root := scaffoldProject(t, "postgres://u:p@localhost/db")
	before := snapshotTree(t, root)
	output, err := runScaffold(root, "post", "title:string", "body:text?", "published:bool", "--tenant-scoped", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatalf("--dry-run changed the project:\nbefore %v\nafter  %v", before, after)
	}
	for _, want := range []string{
		"would create  db/migrations/20261014120000_create_posts.up.sql",
		"would create  " + filepath.Join("app", "domain", "post.go"),
		"would create  " + filepath.Join("app", "adapters", "httpapi", "post.go"),
		"would create  " + filepath.Join("app", "adapters", "htmlui", "post.go"),
		"would create  " + filepath.Join("app", "adapters", "htmlui", "post_test.go"),
		"would create  " + filepath.Join("app", "views", "posts", "index.gohtml"),
		"would create  " + filepath.Join("app", "views", "posts", "show.gohtml"),
		"would create  " + filepath.Join("app", "views", "posts", "new.gohtml"),
		"would create  " + filepath.Join("app", "views", "posts", "edit.gohtml"),
		"would create  " + filepath.Join("app", "views", "posts", "_form.gohtml"),
		"would create  " + filepath.Join("app", "views", "posts", "not_found.gohtml"),
		// The project has no views package yet: the base comes with the scaffold.
		"would create  " + filepath.Join("app", "views", "views.go"),
		"would create  " + filepath.Join("app", "views", "layouts", "application.gohtml"),
		"would create  " + filepath.Join("app", "views", "shared", "_flash.gohtml"),
		"would create  " + filepath.Join("config", "locales", "posts.en.json"),
		// ...and so does config/locales.
		"would create  " + filepath.Join("config", "locales", "locales.go"),
		"would create  " + filepath.Join("config", "locales", "en.json"),
		"htmlui.RegisterPosts(app, postService, renderer, protectCookie)",
		"httpapi.RegisterPosts(app, postService, protect)",
		"middleware.AuthenticateBrowser(\"/session/new\", guarded)",
		"renderer, err := views.New(views.WithI18n(translator))",
		"Forge did not edit app/bootstrap/bootstrap.go. Wire the scaffold there",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output is missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "WARNING") {
		t.Errorf("the project installs the browser pipeline, but the output warns:\n%s", output)
	}
	if strings.Contains(output, "  create  ") {
		t.Errorf("dry-run claims to have created files:\n%s", output)
	}
}

func TestScaffoldWritesTheResourceAndThePagesAndReusesTheViewsBase(t *testing.T) {
	t.Parallel()
	root := scaffoldProject(t, "sqlite:storage/development.db")
	if _, err := runScaffold(root, "post", "title:string", "--global"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"app/domain/post.go", "app/application/post.go", "app/adapters/persistence/post.go", "app/adapters/httpapi/post.go",
		"app/adapters/htmlui/post.go", "app/adapters/htmlui/post_test.go",
		"app/views/posts/index.gohtml", "app/views/views.go", "app/views/views_test.go",
	} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("%s: %v", path, err)
		} else if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", path, info.Mode().Perm())
		}
	}
	viewsGo := readFile(t, filepath.Join(root, "app", "views", "views.go"))
	// The second scaffold finds the views package and leaves it alone.
	output, err := runScaffold(root, "comment", "text:text", "--global")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "views.go") || strings.Contains(output, "application.gohtml") {
		t.Errorf("the second scaffold touches the views base:\n%s", output)
	}
	if readFile(t, filepath.Join(root, "app", "views", "views.go")) != viewsGo {
		t.Error("the views package changed")
	}
	// The wiring builds the renderer for every scaffold; the application does it once.
	if !strings.Contains(output, "htmlui.RegisterComments(app, commentService, renderer, protect)") {
		t.Errorf("wiring:\n%s", output)
	}
}

func TestScaffoldRefusesToOverwriteAnyFile(t *testing.T) {
	t.Parallel()
	for _, existing := range []string{
		"app/adapters/htmlui/post.go", "app/adapters/htmlui/post_test.go",
		"app/views/posts/index.gohtml", "app/views/posts/show.gohtml", "app/views/posts/new.gohtml",
		"app/views/posts/edit.gohtml", "app/views/posts/_form.gohtml", "app/views/posts/not_found.gohtml",
		"app/domain/post.go", "app/adapters/httpapi/post.go",
	} {
		t.Run(existing, func(t *testing.T) {
			t.Parallel()
			root := scaffoldProject(t, "sqlite:storage/development.db")
			writeFile(t, filepath.Join(root, filepath.FromSlash(existing)), "hand-written\n")
			before := snapshotTree(t, root)
			_, err := runScaffold(root, "post", "title:string", "--global")
			if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") || !strings.Contains(err.Error(), filepath.FromSlash(existing)) {
				t.Fatalf("err = %v, want a refusal naming %s", err, existing)
			}
			if after := snapshotTree(t, root); !slices.Equal(before, after) {
				t.Fatalf("a refused run left files behind:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

func TestScaffoldRefusesAForeignViewsPackageAndPluralsThatCollideWithRoutes(t *testing.T) {
	t.Parallel()
	root := scaffoldProject(t, "sqlite:storage/development.db")
	writeFile(t, filepath.Join(root, "app", "views", "views.go"), "package views // mine\n")
	before := snapshotTree(t, root)
	if _, err := runScaffold(root, "post", "title:string", "--global"); err == nil || !strings.Contains(err.Error(), "views.go") {
		t.Fatalf("err = %v, want a refusal to use the foreign views package", err)
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatal("a refused run left files behind")
	}

	clean := scaffoldProject(t, "sqlite:storage/development.db")
	for _, plural := range scaffoldReservedPlurals {
		if _, err := runScaffold(clean, "thing", "name:string", "--global", "--plural", plural); err == nil || !strings.Contains(err.Error(), "already uses") {
			t.Errorf("plural %q: err = %v", plural, err)
		}
	}
}

// A scaffold is a resource and pages: what `generate resource` writes must be
// byte for byte what the scaffold writes for the same resource.
func TestScaffoldGeneratesTheSameResourceFilesAsTheResourceCommand(t *testing.T) {
	t.Parallel()
	args := []string{"order_item", "sku:string", "note:text?", "quantity:int", "--tenant-scoped"}
	resourceRoot := scaffoldProject(t, "mysql://u:p@localhost/db")
	if _, err := runResource(resourceRoot, args...); err != nil {
		t.Fatal(err)
	}
	scaffoldRoot := scaffoldProject(t, "mysql://u:p@localhost/db")
	if _, err := runScaffold(scaffoldRoot, args...); err != nil {
		t.Fatal(err)
	}
	for _, path := range snapshotTree(t, resourceRoot) {
		info, err := os.Stat(filepath.Join(resourceRoot, path))
		if err != nil || info.IsDir() {
			continue
		}
		if readFile(t, filepath.Join(resourceRoot, path)) != readFile(t, filepath.Join(scaffoldRoot, path)) {
			t.Errorf("%s differs between generate resource and generate scaffold", path)
		}
	}
	if _, err := os.Stat(filepath.Join(resourceRoot, "app", "adapters", "htmlui")); !os.IsNotExist(err) {
		t.Errorf("generate resource created HTML pages: %v", err)
	}
}

func TestScaffoldPagesFollowTheFieldTypes(t *testing.T) {
	t.Parallel()
	spec := resourceSpec{
		resourceRequest: resourceRequest{
			Name: "ticket", Plural: "tickets", Scaffold: true,
			Fields: []resourceField{
				{"subject", "string", false}, {"details", "text", true}, {"priority", "int", false}, {"urgent", "bool", false},
				{"escalated", "bool", true}, {"due_at", "time", true}, {"owner_id", "uuid", false},
			},
		},
		Module: "example.com/shop", Database: "sqlite",
	}
	view := newScaffoldView(spec)
	form, err := renderScaffoldTemplate("_form.gohtml.tmpl", view)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<input type="text" maxlength="255" id="ticket_subject" name="subject" value="{{.Values.Subject}}" required`,
		`<textarea id="ticket_details" name="details" rows="6"{{if`,
		`<input type="number" step="1" inputmode="numeric" id="ticket_priority" name="priority" value="{{.Values.Priority}}" required`,
		`<input type="checkbox" id="ticket_urgent" name="urgent" value="on"{{if .Values.Urgent}} checked{{end}}`,
		`<select id="ticket_escalated" name="escalated"`,
		`<input type="datetime-local" step="any" id="ticket_due_at" name="due_at" value="{{.Values.DueAt}}"{{if`,
		`pattern="[0-9a-fA-F]{8}-`,
		`name="owner_id" value="{{.Values.OwnerID}}" required`,
		`{{csrfField}}`, `{{with .Method}}{{methodField .}}{{end}}`,
	} {
		if !strings.Contains(form, want) {
			t.Errorf("the form is missing %q:\n%s", want, form)
		}
	}
	// Optional fields may be left empty: the browser must not insist.
	for _, optional := range []string{"details", "escalated", "due_at"} {
		_, after, _ := strings.Cut(form, `name="`+optional+`"`)
		tag, _, _ := strings.Cut(after, ">")
		if strings.Contains(tag, "required") {
			t.Errorf("optional field %s is required by the browser: %s", optional, tag)
		}
	}
	show, err := renderScaffoldTemplate("show.gohtml.tmpl", view)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`{{methodField "DELETE"}}`, `{{csrfField}}`, `name="version" value="{{.Item.Version}}"`, `<pre>{{.}}</pre>`} {
		if !strings.Contains(show, want) {
			t.Errorf("the show page is missing %q:\n%s", want, show)
		}
	}
	index, err := renderScaffoldTemplate("index.gohtml.tmpl", view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(index, "Details") {
		t.Errorf("long text must not be an index column:\n%s", index)
	}
}

func TestScaffoldWiringKeepsThePagesClosed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		database     string
		identity     bool
		tenantScoped bool
		want         []string
		not          []string
	}{
		{"identity tenant", "postgres", true, true, []string{"htmlui.RegisterPosts(app, postService, renderer, protectCookie)", "middleware.AuthenticateBrowser(\"/session/new\", guarded)", "AuthenticateBrowser"}, []string{"protectPlatformWrite"}},
		{"identity global", "postgres", true, false, []string{
			"htmlui.RegisterPosts(app, postService, renderer, func(permission string, next http.Handler)",
			`permission == "posts:write"`, "return protectCookie(permission, next)", "protectPlatformWrite := func(",
		}, nil},
		{"no identity", "sqlite", false, true, []string{"htmlui.RegisterPosts(app, postService, renderer, protect)", "permission_denied"}, []string{"protectCookie", "auth."}},
	} {
		spec := resourceSpec{
			resourceRequest: resourceRequest{Name: "post", Plural: "posts", TenantScoped: test.tenantScoped, Scaffold: true},
			Module:          "example.com/shop", Database: test.database, Identity: test.identity, BrowserPipeline: true,
		}
		wiring := resourceWiring(spec)
		if strings.Contains(wiring, "return next, nil") {
			t.Errorf("%s: the wiring passes requests through:\n%s", test.name, wiring)
		}
		for _, want := range test.want {
			if !strings.Contains(wiring, want) {
				t.Errorf("%s: the wiring is missing %q:\n%s", test.name, want, wiring)
			}
		}
		for _, not := range test.not {
			if strings.Contains(wiring, not) {
				t.Errorf("%s: the wiring contains %q:\n%s", test.name, not, wiring)
			}
		}
	}
	// Without the CSRF middleware the forms would be unprotected: say so.
	spec := resourceSpec{resourceRequest: resourceRequest{Name: "post", Plural: "posts", Scaffold: true}, Module: "example.com/shop", Database: "sqlite"}
	if wiring := resourceWiring(spec); !strings.Contains(wiring, "WARNING") || !strings.Contains(wiring, "csrf.Protection") {
		t.Errorf("a missing browser pipeline is not reported:\n%s", wiring)
	}
}

func TestScaffoldDetectsTheBrowserPipeline(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, "sqlite:storage/development.db")
	output, err := runScaffold(root, "post", "title:string", "--global", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "WARNING: app/bootstrap/bootstrap.go does not appear to install csrf.Protection") {
		t.Errorf("no warning without a pipeline:\n%s", output)
	}
}

func TestScaffoldRefusesTheFormattingScopesAsPlurals(t *testing.T) {
	t.Parallel()
	root := scaffoldProject(t, "sqlite:storage/development.db")
	before := snapshotTree(t, root)
	for _, plural := range scaffoldFormattingScopes {
		_, err := runScaffold(root, "thing", "name:string", "--global", "--plural", plural)
		// "time" is also a package the generated code imports, which the
		// argument parser refuses first; either way it is refused.
		if err == nil || !strings.Contains(err.Error(), "--plural") || (plural != "time" && !strings.Contains(err.Error(), "formatting scope")) {
			t.Errorf("plural %q: err = %v, want a refusal naming the formatting scopes", plural, err)
		}
	}
	if after := snapshotTree(t, root); !slices.Equal(before, after) {
		t.Fatal("a refused run left files behind")
	}
}

// An application created with --locale pt-BR has no pt-BR text for a
// scaffold unless the scaffold writes it: production would show humanized
// keys. The file starts as the English text, so `forge i18n check` is clean.
func TestScaffoldWritesTheDefaultLocalesFileAndDestroyKeepsItOnlyWhenTranslated(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, locales string
		want          string // the extra file, empty for none
	}{
		{"pt-BR", "const DefaultLocale = \"pt-BR\"\n", "posts.pt-BR.json"},
		{"es", "package locales\n\n// DefaultLocale is x.\nconst DefaultLocale = \"es\"\n", "posts.es.json"},
		{"en", "const DefaultLocale = \"en\"\n", ""},
		{"no locales.go", "", ""},
		{"unreadable constant", "const DefaultLocale = \"not a locale\"\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := scaffoldProject(t, "sqlite:storage/development.db")
			if test.locales != "" {
				writeFile(t, filepath.Join(root, "config", "locales", "locales.go"), test.locales)
			}
			if _, err := runScaffold(root, "post", "title:string", "--global"); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(filepath.Join(root, "config", "locales"))
			if err != nil {
				t.Fatal(err)
			}
			var scoped []string
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "posts.") {
					scoped = append(scoped, entry.Name())
				}
			}
			want := []string{"posts.en.json"}
			if test.want != "" {
				want = append(want, test.want)
			}
			slices.Sort(want)
			if !slices.Equal(scoped, want) {
				t.Fatalf("scoped locale files = %v, want %v", scoped, want)
			}
			if test.want == "" {
				return
			}
			locale := strings.Split(test.want, ".")[1]
			english := readFile(t, filepath.Join(root, "config", "locales", "posts.en.json"))
			text := readFile(t, filepath.Join(root, "config", "locales", test.want))
			if strings.ReplaceAll(text, `"`+locale+`"`, `"en"`) != english {
				t.Errorf("%s is not the English text under %q:\n%s", test.want, locale, text)
			}

			// Unchanged: both go (with the migrations, so it can be generated
			// again below).
			if _, err := destroy(root, "scaffold", "post", "title:string", "--global", "--migrations"); err != nil {
				t.Fatal(err)
			}
			for _, name := range want {
				if _, err := os.Stat(filepath.Join(root, "config", "locales", name)); !os.IsNotExist(err) {
					t.Errorf("%s survived the destroy: %v", name, err)
				}
			}

			// Translated since: destroy keeps the translation, removes the
			// rest, and says so.
			if _, err := runScaffold(root, "post", "title:string", "--global"); err != nil {
				t.Fatal(err)
			}
			translated := strings.Replace(text, "Back to posts", "Voltar", 1)
			writeFile(t, filepath.Join(root, "config", "locales", test.want), translated)
			out, err := destroy(root, "scaffold", "post", "title:string", "--global")
			if err != nil {
				t.Fatalf("destroy with a translated %s: %v", test.want, err)
			}
			if !strings.Contains(out, "keep") || !strings.Contains(out, test.want) {
				t.Errorf("destroy output does not report the kept translation:\n%s", out)
			}
			if got := readFile(t, filepath.Join(root, "config", "locales", test.want)); got != translated {
				t.Errorf("destroy changed the translated %s:\n%s", test.want, got)
			}
			if _, err := os.Stat(filepath.Join(root, "config", "locales", "posts.en.json")); !os.IsNotExist(err) {
				t.Errorf("posts.en.json survived the destroy: %v", err)
			}
		})
	}
}
