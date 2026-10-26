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

const authenticationUsage = "usage: forge generate authentication [--api-auth] [--dry-run]"

// runGenerateAuthentication is `forge generate authentication`: the sign-in,
// sign-out, two-factor and password-reset pages `forge new -d postgresql`
// generates, for an application that lacks them. Like the other generators
// it never overwrites a file and never edits bootstrap.go or main.go; it
// prints the wiring instead.
func runGenerateAuthentication(args []string, stdout io.Writer, root string, env []string) error {
	dryRun, apiAuth := false, false
	for _, arg := range args {
		switch {
		case arg == "--dry-run" && !dryRun:
			dryRun = true
		case arg == "--api-auth" && !apiAuth:
			apiAuth = true
		default:
			return errors.New(authenticationUsage)
		}
	}
	plan, err := planAuthentication(root, env, apiAuth)
	if err != nil {
		return err
	}
	if plan.pagesPresent {
		// --api-auth in an application that has the pages: nothing to
		// write, only the JSON API wiring to print.
		_, err = io.WriteString(stdout, apiAuthOnlyWiring(plan))
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
	i18nMiddleware  bool
	// apiAuth is --api-auth: the wiring also mounts the JSON
	// authentication APIs, as `forge new --api-auth` does.
	apiAuth bool
	// jsonAuthRoutes are the JSON authentication routes bootstrap.go
	// already mounts (applications from before --api-auth mount them all).
	jsonAuthRoutes []string
	// pagesPresent is set for --api-auth when every page file exists
	// already (an application from forge new): files is then empty and only
	// the JSON API wiring is printed.
	pagesPresent bool
}

func planAuthentication(root string, env []string, apiAuth bool) (authenticationPlan, error) {
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
	plan := authenticationPlan{module: module, command: "<name>", apiAuth: apiAuth, browserPipeline: hasBrowserPipeline(projectRoot), i18nMiddleware: hasI18nMiddleware(projectRoot), jsonAuthRoutes: mountedJSONAuthRoutes(projectRoot)}
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
	if apiAuth && allRegularFiles(projectRoot, authentication) {
		plan.pagesPresent = true
		return plan, nil
	}
	for _, file := range authentication {
		plan.files = append(plan.files, generatedFile{path: file.Path, content: string(file.Content)})
	}
	base, err := viewBaseFiles(projectRoot, module)
	if err != nil {
		return authenticationPlan{}, errors.Join(errors.New("the pages need the application's views package"), err)
	}
	plan.files = append(plan.files, base...)
	// The pages' text in the other locales the application has and Forge
	// ships translations for.
	for _, locale := range scaffold.Locales {
		if locale == "en" {
			continue
		}
		if _, err := projectRoot.Lstat(filepath.Join(localesDir, locale+".json")); err != nil {
			continue
		}
		translation, err := scaffold.AuthenticationTranslation(locale)
		if err != nil {
			return authenticationPlan{}, err
		}
		plan.files = append(plan.files, generatedFile{path: translation.Path, content: string(translation.Content)})
	}

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

// allRegularFiles reports whether every file exists in the application as a
// regular file (not a symbolic link or a directory).
func allRegularFiles(projectRoot *os.Root, files []scaffold.BaseFile) bool {
	for _, file := range files {
		info, err := projectRoot.Lstat(file.Path)
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return len(files) > 0
}

// apiAuthOnlyWiring is printed for --api-auth when the application already
// has the sign-in pages: Forge writes nothing and prints the JSON API wiring.
func apiAuthOnlyWiring(plan authenticationPlan) string {
	var out strings.Builder
	out.WriteString("\nThe sign-in pages already exist (app/adapters/htmlauth, app/views/{sessions,passwords,mfa}); nothing was written.\n")
	if len(plan.jsonAuthRoutes) == len(jsonAuthRoutePatterns) {
		out.WriteString("\n  JSON authentication APIs (--api-auth): bootstrap.go already mounts them; keep them.\n")
		return out.String()
	}
	if len(plan.jsonAuthRoutes) > 0 {
		fmt.Fprintf(&out, "\n  app/bootstrap/bootstrap.go already mounts %s; add only the others below.\n", strings.Join(plan.jsonAuthRoutes, ", "))
	}
	out.WriteString("\nForge did not edit app/bootstrap/bootstrap.go. Mount the JSON authentication APIs there:\n")
	out.WriteString(apiAuthWiring)
	return out.String()
}

// jsonAuthRoutePatterns are the JSON authentication routes `forge new
// --api-auth` mounts (apiAuthRoutes in the generated bootstrap.go).
var jsonAuthRoutePatterns = []string{
	"POST /auth/login", "POST /auth/logout", "POST /auth/mfa/complete",
	"POST /auth/recovery/request", "POST /auth/recovery/reset",
	"POST /auth/cookie/login", "POST /auth/cookie/mfa/complete", "POST /auth/cookie/logout",
	"GET /v1/auth/session",
}

// mountedJSONAuthRoutes lists the JSON authentication routes that
// app/bootstrap/bootstrap.go registers, as quoted route patterns.
func mountedJSONAuthRoutes(projectRoot *os.Root) []string {
	source, err := projectRoot.ReadFile(filepath.Join("app", "bootstrap", "bootstrap.go"))
	if err != nil {
		return nil
	}
	var mounted []string
	for _, pattern := range jsonAuthRoutePatterns {
		if strings.Contains(string(source), `"`+pattern+`"`) {
			mounted = append(mounted, pattern)
		}
	}
	return mounted
}

// apiAuthWiring mounts the JSON authentication APIs as `forge new
// --api-auth` does; it is printed for --api-auth.
const apiAuthWiring = `
  JSON authentication APIs (--api-auth): every route below is another place
  a password or a one-time code is entered, so mount only those API clients
  or browser scripts use. Inside identityRoutes, after htmlauth.Register:

	loginHandler, err := auth.NewLoginHandler(login) // bearer sessions
	if err != nil {
		return err
	}
	logoutHandler, err := auth.NewAuditedLogoutHandler(repository, repository)
	if err != nil {
		return err
	}
	if err := app.Handle("POST /auth/login", loginHandler); err != nil {
		return err
	}
	if err := app.Handle("POST /auth/logout", logoutHandler); err != nil {
		return err
	}
	cookieLoginHandler, err := auth.NewCookieLoginHandler(login) // browser scripts
	if err != nil {
		return err
	}
	cookieLogoutHandler, err := auth.NewAuditedCookieLogoutHandler(repository, repository)
	if err != nil {
		return err
	}
	if err := app.Handle("POST /auth/cookie/login", cookieLoginHandler); err != nil {
		return err
	}
	if err := app.Handle("POST /auth/cookie/logout", cookieLogoutHandler); err != nil {
		return err
	}

     With MFA, also mount auth.NewMFACompletionHandler(mfaService) at
     POST /auth/mfa/complete and auth.NewCookieMFACompletionHandler(mfaService)
     at POST /auth/cookie/mfa/complete; for JSON password recovery,
     auth.NewAuditedPasswordRecoveryRequestHandler(recoveryService, repository)
     and auth.NewAuditedPasswordResetHandler(recoveryService, repository) at
     POST /auth/recovery/request and /auth/recovery/reset. A new application
     shows the complete function: forge new NAME --api-auth (apiAuthRoutes in
     app/bootstrap/bootstrap.go).
`

// authenticationWiring is printed after generation (and for --dry-run).
func authenticationWiring(plan authenticationPlan) string {
	var out strings.Builder
	main := filepath.Join("cmd", plan.command, "main.go")
	fmt.Fprintf(&out, "\nForge did not edit app/bootstrap/bootstrap.go or %s. Wire the pages there:\n\n", main)
	out.WriteString("  1. Add these imports to app/bootstrap/bootstrap.go (skip any already present):\n\n")
	fmt.Fprintf(&out, "\t%q\n\t%q\n", plan.module+"/app/adapters/htmlauth", plan.module+"/app/views")
	out.WriteString(`
  2. Inside identityRoutes, after the login, MFA (when configured) and recovery
     services are built, mount the pages. They set the session cookie that
     middleware.AuthenticateCookie and AuthenticateBrowser read; reuse an
     existing renderer if you have one:

	browser, err := auth.NewBrowser(auth.BrowserOptions{
		Login:     login,
		Sessions:  repository,
		MFA:       mfaService,
		Passwords: repository,
		Recovery:  recoveryService,
		Auditor:   repository,
	})
	if err != nil {
		return err
	}
	renderer, err := views.New(views.WithI18n(translator))
	if err != nil {
		return err
	}
	middleware, err := auth.NewMiddleware(repository) // reuse yours if built
	if err != nil {
		return err
	}
	if err := htmlauth.Register(app, browser, renderer, middleware); err != nil {
		return err
	}

     Reset requests answer after at least 100 ms and are processed off the
     request path. If your recovery service predates this release, build it as
     forge new does: with auth.WithRecoveryDispatcher(recoveryQueue), a queue
     from auth.NewRecoveryQueue that you Start with the service and stop with
     app.OnShutdown (or auth/recoveryqueue on a persistent queue), and
     authpostgres.NewRecoveryThrottler. Run migrate: the throttle table gained
     a counter scope (docs/ACCOUNT_RECOVERY.md).

  3. Send signed-out browsers to the sign-in page instead of a JSON 401: guard
     HTML pages with

	guarded, err := middleware.AuthenticateBrowser(htmlauth.SignInPath, page)

     in place of middleware.AuthenticateCookie(page) (the protectCookie guard
     that forge generate scaffold prints already does).

`)
	fmt.Fprintf(&out, "  4. Add the setup command to the commands in %s, so the first owner\n     can be created:\n\n\tauthpostgres.SetupCommand(),\n", main)
	switch {
	case plan.apiAuth && len(plan.jsonAuthRoutes) == len(jsonAuthRoutePatterns):
		out.WriteString("\n  JSON authentication APIs (--api-auth): bootstrap.go already mounts them; keep them.\n")
	case plan.apiAuth:
		out.WriteString(apiAuthWiring)
	case len(plan.jsonAuthRoutes) > 0:
		fmt.Fprintf(&out, "\n  NOTE: app/bootstrap/bootstrap.go also mounts JSON authentication routes (%s). forge new mounts them only with --api-auth: with the pages above, browsers need none of them, and each is another place a password or code is entered. Remove those no API client or script uses (run this command with --api-auth to keep them).\n", strings.Join(plan.jsonAuthRoutes, ", "))
	}
	if !plan.browserPipeline {
		out.WriteString("\n  WARNING: app/bootstrap/bootstrap.go does not appear to install csrf.Protection, web.MethodOverride and web.Flash (forge new generates useBrowserPipeline for them). auth.Browser refuses every form without a verified CSRF token, so sign-in cannot work until they are installed (docs/VIEWS.md).\n")
	}
	if !plan.i18nMiddleware {
		out.WriteString("\n  WARNING: app/bootstrap/bootstrap.go does not appear to build the translator and install its middleware (forge new does: `translator, err := locales.New(app.Config(), app.Logger())` in Configure, passed to identityRoutes, and `translator.Middleware(i18n.NegotiationOptions{})` first in useBrowserPipeline). The pages' text comes from config/locales and needs them (docs/I18N.md).\n")
	}
	out.WriteString("\n  Then: forge test && forge setup --tenant acme --email you@example.com\n")
	return out.String()
}
