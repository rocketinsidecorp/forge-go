package cli

import (
	"embed"
	"errors"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

const controllerUsage = "usage: forge generate controller NAME ACTION... [--api] [--public] [--dry-run]"

const maxControllerActions = 20

// controllerWriteActions are the actions that change data. A controller has no
// model behind it, so it generates read-only pages; the actions that save
// belong to `forge generate scaffold`, which also generates the model, the
// form and the CSRF-protected routes they need.
var controllerWriteActions = []string{"create", "update", "destroy"}

//go:embed controllertemplates/*.tmpl
var controllerTemplates embed.FS

// controllerRequest is a syntactically valid `generate controller` command line.
type controllerRequest struct {
	Name    string
	Actions []string
	// API generates JSON handlers under /v1 instead of HTML pages, like
	// `forge new --api`.
	API bool
	// Public mounts the routes without a guard. Without it the routes need an
	// application-supplied guard and a permission, as a resource's do.
	Public  bool
	DryRun  bool
	Destroy bool
}

// controllerSpec is a request resolved against an application.
type controllerSpec struct {
	controllerRequest
	Module string
	// Identity reports that the application has Forge identity: a guarded
	// controller then gets its permission from a migration and authenticates
	// the session cookie (HTML) or the bearer token (JSON).
	Identity bool
	// BearerSignIn reports that bootstrap.go mounts the bearer-token login
	// (--api-auth); without it a JSON controller is wired closed.
	BearerSignIn bool
	// NoDatabase reports an application created without a database, whose
	// Configure only calls routes.
	NoDatabase bool
	// ViewRenderer reports that bootstrap.go already builds the view renderer.
	ViewRenderer bool
	// AddsViewBase reports that the controller also adds the views base.
	AddsViewBase bool
	// Seam reports that bootstrap.go calls the routes seam
	// (app/bootstrap/routes_gen.go), where the controller is wired.
	Seam bool
	seam routesSeam
	// PermissionOwner is the migration that already registers NAME:read (a
	// model, resource or scaffold whose plural is NAME): the controller then
	// reuses that permission and generates no migration of its own.
	PermissionOwner string
}

// parseControllerArgs reads `NAME ACTION... [--api] [--public] [--dry-run]`.
// It touches no files.
func parseControllerArgs(args []string) (controllerRequest, error) {
	flags, positional, err := splitFlags(args, controllerUsage, "--api", "--public", "--dry-run")
	if err != nil {
		return controllerRequest{}, err
	}
	request := controllerRequest{API: flags["--api"], Public: flags["--public"], DryRun: flags["--dry-run"]}
	if len(positional) == 0 {
		return controllerRequest{}, usageError{controllerUsage}
	}
	request.Name = positional[0]
	if err := validateControllerName(request.Name); err != nil {
		return controllerRequest{}, err
	}
	if len(positional) == 1 {
		return controllerRequest{}, errors.New("declare at least one action, for example: index")
	}
	if len(positional)-1 > maxControllerActions {
		return controllerRequest{}, fmt.Errorf("a controller has at most %d actions", maxControllerActions)
	}
	seen := make(map[string]bool)
	for _, action := range positional[1:] {
		if err := validateControllerAction(action); err != nil {
			return controllerRequest{}, err
		}
		// Two names that export to one identifier (id and i_d) would declare
		// one method twice.
		exported := exportedName(action)
		if seen[exported] {
			return controllerRequest{}, fmt.Errorf("action %q is declared twice (or exports to the same Go method as another action)", action)
		}
		seen[exported] = true
		request.Actions = append(request.Actions, action)
	}
	return request, nil
}

func validateControllerName(name string) error {
	if err := rejectSeamReservedName("controller name", name); err != nil {
		return err
	}
	switch {
	case len(name) > maxGeneratorNameLength || !snakeName.MatchString(name):
		return fmt.Errorf("controller name %q must be snake_case, usually plural (lowercase letters and digits, words separated by single underscores, starting with a letter, at most %d characters)", name, maxGeneratorNameLength)
	case strings.HasSuffix(name, "_controller"):
		return fmt.Errorf("controller name %q ends in _controller, which Forge adds to the Go names; name it %q", name, strings.TrimSuffix(name, "_controller"))
	case slices.Contains(scaffoldReservedPlurals, name):
		return fmt.Errorf("controller name %q is a path the application or its view tree already uses (%s); choose another name", name, strings.Join(scaffoldReservedPlurals, ", "))
	}
	return nil
}

func validateControllerAction(action string) error {
	switch {
	case len(action) > maxGeneratorNameLength || !snakeName.MatchString(action):
		return fmt.Errorf("action %q must be snake_case (lowercase letters and digits, words separated by single underscores, starting with a letter, at most %d characters)", action, maxGeneratorNameLength)
	case slices.Contains(controllerWriteActions, action):
		return fmt.Errorf("action %q changes data, and a controller has no model: use `forge generate scaffold` for create, update and destroy, or write the handler yourself", action)
	}
	return nil
}

// runGenerateController is `forge generate controller`: HTTP handlers for the
// named actions, their routes, views (HTML) and handler tests, without any
// persistence.
func runGenerateController(args []string, stdout io.Writer, root string, env []string, now time.Time) error {
	request, err := parseControllerArgs(args)
	if err != nil {
		return err
	}
	spec, files, err := planController(root, request, env, now)
	if err != nil {
		return err
	}
	if !spec.Seam {
		if !request.DryRun {
			if err := applyGeneratedFiles(root, directoriesOf(files), files); err != nil {
				return err
			}
		}
		return reportFiles(stdout, request.DryRun, files, controllerWiring(spec))
	}
	// The seam is planned first, so a refused seam writes nothing.
	wiring, err := planControllerSeam(root, spec, false)
	if err != nil {
		return err
	}
	if !request.DryRun {
		if err := applyGeneratedFiles(root, directoriesOf(newFiles(files)), files); err != nil {
			return err
		}
		if wiring.verb == "wire" {
			if err := writeRoutes(root, wiring.content); err != nil {
				return err
			}
		}
	}
	return reportFiles(stdout, request.DryRun, files, reportSeam(wiring, request.DryRun, controllerThen(spec)))
}

// planControllerSeam plans the seam with the controller's routes added (or,
// for `forge destroy`, removed).
func planControllerSeam(root string, spec controllerSpec, remove bool) (seamWiring, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return seamWiring{}, err
	}
	defer func() { _ = projectRoot.Close() }()
	parts := buildControllerWiring(spec)
	content, verb, err := planRoutes(projectRoot, spec.seam, routesChange{
		entry:  scaffold.RouteBlock{Kind: "controller", Name: spec.Name, Body: parts.Usage},
		shared: sharedFromParts(parts),
		remove: remove,
	})
	return seamWiring{content: content, verb: verb, notes: parts.Notes}, err
}

// controllerView is everything the controller templates read.
type controllerView struct {
	Name    string // pages
	Title   string // Pages
	Camel   string // pages
	Module  string
	API     bool
	Public  bool
	Actions []controllerActionView
}

type controllerActionView struct {
	Name       string // about_us
	Title      string // AboutUs
	Camel      string // aboutUs
	HumanTitle string // About us
	Pattern    string // GET /pages/about_us
	Path       string // /pages/about_us
}

func newControllerView(spec controllerSpec) controllerView {
	view := controllerView{
		Name: spec.Name, Title: exportedName(spec.Name), Camel: lowerCamel(spec.Name),
		Module: spec.Module, API: spec.API, Public: spec.Public,
	}
	base := "/" + spec.Name
	if spec.API {
		base = "/v1/" + spec.Name
	}
	for _, action := range spec.Actions {
		path := base + "/" + action
		if action == "index" {
			path = base
		}
		view.Actions = append(view.Actions, controllerActionView{
			Name: action, Title: exportedName(action), Camel: lowerCamel(action),
			HumanTitle: capitalize(strings.ReplaceAll(action, "_", " ")),
			Pattern:    "GET " + path, Path: path,
		})
	}
	return view
}

// controllerDirectory is where the handlers and their tests live.
func controllerDirectory(api bool) string {
	if api {
		return filepath.Join("app", "adapters", "httpapi")
	}
	return filepath.Join("app", "adapters", "htmlui")
}

// planController resolves request against the application in root and renders
// every file. Nothing is written.
func planController(root string, request controllerRequest, env []string, now time.Time) (controllerSpec, []generatedFile, error) {
	projectRoot, module, err := openApplication(root)
	if err != nil {
		return controllerSpec{}, nil, err
	}
	defer func() { _ = projectRoot.Close() }()
	if !request.API && !request.Destroy && apiOnlyApplication(projectRoot) {
		return controllerSpec{}, nil, errors.New(apiOnlyControllerRefusal)
	}
	adapter, err := databaseAdapter(root, env)
	if err != nil {
		return controllerSpec{}, nil, errors.New("environment files could not be loaded: check .env.development and .env.local syntax")
	}
	spec := controllerSpec{controllerRequest: request, Module: module}
	spec.Identity = adapter == "postgres" && hasIdentityMigrations(projectRoot) && !apiOnlyApplication(projectRoot)
	spec.BearerSignIn = spec.Identity && hasBearerSignIn(projectRoot)
	spec.NoDatabase = adapter == "none"
	spec.seam = openRoutesSeam(projectRoot, module, adapter, spec.Identity)
	spec.Seam = spec.seam.wired
	if !request.API {
		// The seam builds the pages' renderer itself.
		spec.ViewRenderer = hasViewRenderer(projectRoot) && !spec.Seam
	}

	view := newControllerView(spec)
	templatePrefix := "controller_html"
	if request.API {
		templatePrefix = "controller_api"
	}
	var files []generatedFile
	for _, source := range []struct{ suffix, template string }{
		{"_controller.go", templatePrefix + ".go.tmpl"},
		{"_controller_test.go", templatePrefix + "_test.go.tmpl"},
	} {
		content, err := renderEmbedded(controllerTemplates, "controllertemplates/"+source.template, view)
		if err != nil {
			return controllerSpec{}, nil, err
		}
		path := filepath.Join(controllerDirectory(request.API), request.Name+source.suffix)
		formatted, err := format.Source([]byte(content))
		if err != nil {
			return controllerSpec{}, nil, fmt.Errorf("format generated %s: %w", path, err)
		}
		files = append(files, generatedFile{path: path, content: string(formatted)})
	}
	if !request.API {
		for _, action := range view.Actions {
			files = append(files, generatedFile{path: filepath.Join("app", "views", request.Name, action.Name+".gohtml"), content: generatedView})
		}
		if !request.Destroy {
			base, err := viewBaseFiles(projectRoot, module)
			if err != nil {
				return controllerSpec{}, nil, errors.Join(errors.New("the pages need the application's views package"), err)
			}
			spec.AddsViewBase = len(base) > 0
			files = append(files, base...)
		}
	}
	if spec.Identity && !request.Public {
		migrationName := "create_" + request.Name + "_controller_permissions"
		if !request.Destroy {
			// A permission has one owner migration, whose rollback removes it:
			// reuse NAME:read when another migration registers it.
			spec.PermissionOwner, _, err = permissionRegisteredBy(projectRoot, migrationName, request.Name+":read")
			if err != nil {
				return controllerSpec{}, nil, err
			}
		}
		if spec.PermissionOwner == "" {
			up, down := controllerPermissionMigration(request.Name)
			migrations, existing, err := planMigrationPair(projectRoot, migrationName, request.Destroy, now, up, down)
			if err != nil {
				return controllerSpec{}, nil, err
			}
			if existing != "" {
				// The same command run again finds its own migration, unchanged.
				if migrations = identicalMigrationPair(projectRoot, migrationName, now, up, down); migrations == nil || !allIdentical(projectRoot, append(slices.Clone(migrations), files...)) {
					return controllerSpec{}, nil, fmt.Errorf("refusing to generate: %s already registers the permission of the %s controller", existing, request.Name)
				}
			}
			files = append(migrations, files...)
		}
	}
	if err := checkRerunnableFiles(projectRoot, files, request.Destroy); err != nil {
		return controllerSpec{}, nil, err
	}
	return spec, files, nil
}

// controllerPermissionMigration registers the permission a guarded controller
// asks for in an application with Forge identity, and gives it to the owner
// role like a resource's migration does. The migration owns the permission:
// the controller generates it only when no other migration registers NAME:read,
// and no generator registers that permission again while it exists. Its up is
// idempotent; its down takes back only what the up gave, the owner roles'
// grant and the permission, and fails (changing nothing, because
// forge_role_permissions restricts the delete) while another role still holds
// the permission, instead of silently revoking grants an administrator made.
func controllerPermissionMigration(name string) (up, down string) {
	permission := name + ":read"
	up = fmt.Sprintf(`-- Permission that guards the %[1]s controller's routes (the protect argument of Register%[2]sController).
-- This migration owns it: its rollback removes it.
INSERT INTO forge_permissions (name) VALUES ('%[3]s')
ON CONFLICT (name) DO NOTHING;

-- The owner role (created by `+"`setup`"+`) holds every permission.
INSERT INTO forge_role_permissions (role_id, permission)
SELECT r.id, p.name FROM forge_roles r CROSS JOIN (VALUES ('%[3]s')) AS p (name)
WHERE r.name = 'owner'
ON CONFLICT (role_id, permission) DO NOTHING;
`, name, exportedName(name), permission)
	down = fmt.Sprintf(`-- Removes the permission this migration registered and the owner role's grant
-- of it. While another role still holds %[1]s the delete is restricted and the
-- rollback fails without changing anything: revoke those grants first.
DELETE FROM forge_role_permissions
WHERE permission = '%[1]s' AND role_id IN (SELECT id FROM forge_roles WHERE name = 'owner');
DELETE FROM forge_permissions WHERE name = '%[1]s';
`, permission)
	return up, down
}

// controllerWiring is what `forge generate controller` prints: how to mount
// the routes from app/bootstrap/bootstrap.go, which the generator never edits.
func controllerWiring(spec controllerSpec) string {
	return formatWiring("controller", "controller", buildControllerWiring(spec), controllerThen(spec))
}

// controllerThen is the command to run after generating a controller.
func controllerThen(spec controllerSpec) string {
	if spec.Identity && !spec.Public && spec.PermissionOwner == "" {
		return "forge migrate && forge test"
	}
	return "forge test"
}

func buildControllerWiring(spec controllerSpec) resourceWiringParts {
	title := exportedName(spec.Name)
	adapterPackage := "htmlui"
	if spec.API {
		adapterPackage = "httpapi"
	}
	parts := resourceWiringParts{Imports: []string{spec.Module + "/app/adapters/" + adapterPackage}}
	guard := func(name string) string { return guardReference(spec.Seam, spec.Identity, false, name) }
	parts.Where = "inside Configure, after the `routes(ctx, app)` call and its error check"
	if spec.NoDatabase {
		parts.Where = "inside routes, before its final `return nil`"
	}
	if spec.Identity {
		parts.Where = "inside identityRoutes, after the `middleware, err := auth.NewMiddleware(repository)` statement and its error check"
	}
	arguments := "app"
	if !spec.API {
		arguments += ", renderer"
		if spec.ViewRenderer {
			parts.Notes = append(parts.Notes, "The pages reuse the renderer that bootstrap.go already builds with views.New(views.WithI18n(translator)).")
		} else {
			parts.Imports = append(parts.Imports, spec.Module+"/app/views")
			parts.Renderer = "	renderer, err := views.New(views.WithI18n(translator))\n	if err != nil {\n		return err\n	}\n"
		}
	}
	switch {
	case spec.Public:
		parts.Notes = append(parts.Notes, fmt.Sprintf("WARNING: --public mounts the %s routes without a guard: anyone who can reach the server can call them.", spec.Name))
	case spec.Identity && spec.API && !spec.BearerSignIn:
		parts.Guard = cookieJSONGuard
		parts.Notes = append(parts.Notes, cookieJSONNote)
		arguments += ", " + guard("protect")
	case spec.Identity && spec.API:
		parts.Guard = identityJSONGuard
		arguments += ", " + guard("protect")
	case spec.Identity:
		parts.Guard = identityPagesGuard
		arguments += ", " + guard("protectCookie")
	default:
		parts.Imports = append(parts.Imports, "github.com/rocketinsidecorp/forge-go/fault", "github.com/rocketinsidecorp/forge-go/web")
		parts.Guard = denyAllGuard(fmt.Sprintf("%q", spec.Name+":read"), "")
		parts.Notes = append(parts.Notes, "This application has no Forge identity, so the guard is yours to write; the generator never opens the routes to the public (use --public to say that is what you want).")
		if spec.Seam {
			parts.Guard = seamDenyAllGuard
			parts.Notes[len(parts.Notes)-1] = "This application has no Forge identity, so the routes are mounted closed: protect refuses every request until you write it (copy func protect from app/bootstrap/routes_gen.go into another file of package bootstrap, change it, and run a generator again). Generate the controller with --public to serve it without sign-in."
		}
		arguments += ", protect"
	}
	switch {
	case spec.Identity && !spec.Public && spec.PermissionOwner != "":
		parts.Notes = append(parts.Notes, fmt.Sprintf("The routes reuse the permission %[1]s:read, which %[2]s already registers, so no migration was generated: that migration owns the permission (rolling it back removes it, and the routes then refuse everyone). Grant %[1]s:read to other roles before their callers can use the routes.", spec.Name, filepath.ToSlash(spec.PermissionOwner)))
	case spec.Identity && !spec.Public:
		parts.Notes = append(parts.Notes, fmt.Sprintf("The migration registers the permission %s:read and gives it to the owner role; grant it to other roles before their callers can use the routes.", spec.Name))
	}
	if spec.Identity && !spec.Public && !spec.API {
		parts.Notes = append(parts.Notes, "The pages authenticate the session cookie: a signed-out browser is redirected to /session/new and back, while API clients get the 401 JSON error.")
	}
	parts.Usage = fmt.Sprintf("\tif err := %s.Register%sController(%s); err != nil {\n\t\treturn err\n\t}\n", adapterPackage, title, arguments)
	if spec.Seam {
		toSeam(&parts)
	}
	var routes []string
	for _, action := range newControllerView(spec).Actions {
		routes = append(routes, action.Pattern)
	}
	parts.Notes = append(parts.Notes, "Routes: "+strings.Join(routes, ", ")+". Run `forge routes` to list them.")
	return parts
}
