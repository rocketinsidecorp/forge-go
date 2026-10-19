package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

const authenticationUsage = "usage: forge generate authentication [--dry-run]"

// runGenerateAuthentication is `forge generate authentication`: the sign-in,
// sign-out, two-factor and password-reset pages `forge new -d postgresql`
// generates, for an application that lacks them. Like the other generators
// it never overwrites a file and never edits bootstrap.go or main.go; it
// prints the wiring instead.
func runGenerateAuthentication(args []string, stdout io.Writer, root string, env []string) error {
	dryRun := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "--dry-run":
		dryRun = true
	default:
		return errors.New(authenticationUsage)
	}
	plan, err := planAuthentication(root, env)
	if err != nil {
		return err
	}
	if !dryRun {
		if err := applyGeneratedFiles(root, directoriesOf(plan.files), plan.files); err != nil {
			return err
		}
	}
	verb := "create"
	if dryRun {
		verb = "would create"
	}
	for _, file := range plan.files {
		if _, err := fmt.Fprintf(stdout, "  %s  %s\n", verb, file.path); err != nil {
			return err
		}
	}
	_, err = io.WriteString(stdout, authenticationWiring(plan))
	return err
}

type authenticationPlan struct {
	module          string
	command         string // the cmd/<name> directory, or "<name>" when unknown
	files           []generatedFile
	browserPipeline bool
}

func planAuthentication(root string, env []string) (authenticationPlan, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return authenticationPlan{}, errors.New("go.mod not found: run this inside a Forge application")
	}
	defer func() { _ = projectRoot.Close() }()
	goMod, err := projectRoot.ReadFile("go.mod")
	if err != nil {
		return authenticationPlan{}, errors.New("go.mod not found: run this inside a Forge application")
	}
	module, _, _ := inspectGoMod(goMod)
	if module == "" {
		return authenticationPlan{}, errors.New("go.mod has no module declaration")
	}
	adapter, err := databaseAdapter(root, env)
	if err != nil {
		return authenticationPlan{}, errors.New("environment files could not be loaded: check .env.development and .env.local syntax")
	}
	if adapter != "postgres" {
		database := map[string]string{"mysql": "MySQL or MariaDB", "sqlite": "SQLite", "none": "no database"}[adapter]
		if database == "" {
			database = "a database FORGE_DATABASE_URL does not identify"
		}
		return authenticationPlan{}, fmt.Errorf("forge generate authentication needs PostgreSQL: Forge stores identities (users, tenants, memberships, sessions) only in PostgreSQL today (auth/postgres), and this application uses %s. Create a PostgreSQL application with `forge new NAME -d postgresql`, or implement the auth ports (PasswordStore, CredentialSessionCreator, SessionRevoker) for your database and build the pages on auth.Browser", database)
	}
	if !hasIdentityMigrations(projectRoot) {
		return authenticationPlan{}, errors.New("cmd/<name>/main.go does not compose the Forge identity migrations (authpostgres.Migrations()); the pages need the identity tables: add them to the migrate commands first (see docs/AUTHENTICATION.md)")
	}
	plan := authenticationPlan{module: module, command: "<name>", browserPipeline: hasBrowserPipeline(projectRoot)}
	if name, err := resolveCommand(root, nil); err == nil {
		plan.command = name
	}
	name := plan.command
	if _, err := scaffold.Normalize(scaffold.Options{Name: name}); err != nil {
		name = "app"
	}
	authentication, err := scaffold.AuthenticationFiles(name, module)
	if err != nil {
		return authenticationPlan{}, err
	}
	for _, file := range authentication {
		plan.files = append(plan.files, generatedFile{path: file.Path, content: string(file.Content)})
	}
	base, err := viewBaseFiles(projectRoot, module)
	if err != nil {
		return authenticationPlan{}, errors.Join(errors.New("the pages need the application's views package"), err)
	}
	plan.files = append(plan.files, base...)

	paths := make([]string, 0, len(plan.files))
	for _, file := range plan.files {
		paths = append(paths, file.path)
	}
	if err := rejectExistingSymlinkPaths(projectRoot, paths); err != nil {
		return authenticationPlan{}, err
	}
	for _, path := range paths {
		if _, err := projectRoot.Lstat(path); err == nil {
			return authenticationPlan{}, fmt.Errorf("refusing to overwrite %s; nothing was written", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return authenticationPlan{}, err
		}
	}
	if err := rejectDeclarationConflicts(projectRoot, plan.files); err != nil {
		return authenticationPlan{}, err
	}
	return plan, nil
}

// authenticationWiring is printed after generation (and for --dry-run).
func authenticationWiring(plan authenticationPlan) string {
	var out strings.Builder
	main := filepath.Join("cmd", plan.command, "main.go")
	fmt.Fprintf(&out, "\nForge did not edit app/bootstrap/bootstrap.go or %s. Wire the pages there:\n\n", main)
	out.WriteString("  1. Add these imports to app/bootstrap/bootstrap.go (skip any already present):\n\n")
	fmt.Fprintf(&out, "\t%q\n\t%q\n", plan.module+"/app/adapters/htmlauth", plan.module+"/app/views")
	out.WriteString(`
  2. Inside identityRoutes, after the login, MFA (when configured) and recovery
     services are built, mount the pages. They set the same session cookies as
     POST /auth/cookie/login; reuse an existing renderer if you have one:

	browser, err := auth.NewBrowser(auth.BrowserOptions{
		Login:    login,
		Sessions: repository,
		MFA:      mfaService,
		Recovery: recoveryService,
		Auditor:  repository,
	})
	if err != nil {
		return err
	}
	renderer, err := views.New()
	if err != nil {
		return err
	}
	if err := htmlauth.Register(app, browser, renderer); err != nil {
		return err
	}

  3. Send signed-out browsers to the sign-in page instead of a JSON 401: guard
     HTML pages with

	guarded, err := middleware.AuthenticateBrowser(htmlauth.SignInPath, page)

     in place of middleware.AuthenticateCookie(page) (the protectCookie guard
     that forge generate scaffold prints already does).

`)
	fmt.Fprintf(&out, "  4. Add the setup command to the commands in %s, so the first owner\n     can be created:\n\n\tauthpostgres.SetupCommand(),\n", main)
	if !plan.browserPipeline {
		out.WriteString("\n  WARNING: app/bootstrap/bootstrap.go does not appear to install csrf.Protection, web.MethodOverride and web.Flash (forge new generates useBrowserPipeline for them). auth.Browser refuses every form without a verified CSRF token, so sign-in cannot work until they are installed (docs/VIEWS.md).\n")
	}
	out.WriteString("\n  Then: forge test && forge setup --tenant acme --email you@example.com\n")
	return out.String()
}
