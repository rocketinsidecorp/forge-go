package cli

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

const authenticationUsage = "usage: forge generate authentication [--api-auth] [--dry-run]"

// runGenerateAuthentication is `forge generate authentication` (decision
// D31, like Rails 8's authentication generator): Forge identity for an
// application created without --auth. It writes what `forge new --auth`
// generates (app/bootstrap/identity.go, app/adapters/htmlauth, the pages and
// their translations) and wires it with no edit by hand: app_gen.go composes
// the identity migrations and the setup command, the routes seam mounts the
// pages first, and .env.local gets an MFA key. The application stays
// single-organization; `forge generate tenancy` adds organizations.
func runGenerateAuthentication(args []string, stdout io.Writer, root string, env []string) error {
	dryRun, apiAuth := false, false
	for _, arg := range args {
		switch {
		case arg == "--dry-run" && !dryRun:
			dryRun = true
		case arg == "--api-auth" && !apiAuth:
			apiAuth = true
		default:
			return usageError{authenticationUsage}
		}
	}
	plan, err := planAuthentication(root, env, apiAuth)
	if err != nil {
		return err
	}
	if plan.present {
		// The application has Forge identity already: --api-auth prints
		// the JSON API wiring for identity.go, which is the owner's file.
		_, err = io.WriteString(stdout, apiAuthOnlyWiring(plan))
		return err
	}
	if !dryRun {
		if err := applyGeneratedFiles(root, directoriesOf(plan.files), plan.files); err != nil {
			return err
		}
		if err := writeAuthenticationWiring(root, plan); err != nil {
			return err
		}
	}
	verb, wire := "create", "rewrite"
	if dryRun {
		verb, wire = "would create", "would rewrite"
	}
	var out strings.Builder
	for _, file := range plan.files {
		fmt.Fprintf(&out, "  %s  %s\n", verb, file.path)
	}
	fmt.Fprintf(&out, "  %s  %s\n", wire, scaffold.AppFile)
	fmt.Fprintf(&out, "  %s  %s\n", strings.Replace(wire, "rewrite", "wire", 1), scaffold.RoutesFile)
	if plan.mfaKey != "" {
		fmt.Fprintf(&out, "  %s  .env.local (FORGE_AUTH_MFA_KEY)\n", strings.Replace(wire, "rewrite", "append", 1))
	}
	out.WriteString(authenticationNotes(plan))
	_, err = io.WriteString(stdout, out.String())
	return err
}

type authenticationPlan struct {
	module string
	files  []generatedFile
	// app is app_gen.go with Forge identity, routes the seam with its
	// identity block.
	app, routes []byte
	// mfaKey is the FORGE_AUTH_MFA_KEY line to append to .env.local, empty
	// when it has one.
	mfaKey          string
	browserPipeline bool
	i18nMiddleware  bool
	// present is set when the application has Forge identity already.
	present bool
	// apiAuth is --api-auth.
	apiAuth bool
	// jsonAuthRoutes are the JSON authentication routes the application
	// already mounts.
	jsonAuthRoutes []string
}

// databaseOption is the `forge new -d` name of an adapter.
func databaseOption(adapter string) string {
	switch adapter {
	case "postgres":
		return "postgresql"
	default:
		return adapter
	}
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
	if apiOnlyApplication(projectRoot) {
		return authenticationPlan{}, errors.New(apiOnlyAuthenticationRefusal)
	}
	adapter, err := databaseAdapter(root, env)
	if err != nil {
		return authenticationPlan{}, errors.New("environment files could not be loaded: check .env.development and .env.local syntax")
	}
	switch adapter {
	case "postgres", "mysql", "sqlite":
	default:
		return authenticationPlan{}, errors.New("forge generate authentication needs a database: Forge identity keeps users, organizations and sessions in it, and FORGE_DATABASE_URL selects none. Create the application with a database (forge new NAME --auth, SQLite by default)")
	}
	plan := authenticationPlan{module: module, apiAuth: apiAuth, browserPipeline: hasBrowserPipeline(projectRoot), i18nMiddleware: hasI18nMiddleware(projectRoot), jsonAuthRoutes: mountedJSONAuthRoutes(projectRoot)}
	features, present, err := applicationFeatures(projectRoot)
	if err != nil {
		return authenticationPlan{}, err
	}
	if hasIdentityMigrations(projectRoot, adapter) || (present && features.Identity) {
		if !apiAuth {
			return authenticationPlan{}, errors.New("this application has Forge identity already (its sign-in pages are app/adapters/htmlauth, its wiring app/bootstrap/identity.go); nothing was written. Run it with --api-auth to print the wiring of the JSON authentication APIs")
		}
		plan.present = true
		return plan, nil
	}
	if !present {
		return authenticationPlan{}, fmt.Errorf("%s not found: this application was created before forge generate authentication could wire Forge identity by itself. Create a new one with `forge new NAME --auth` and move your code into it", filepath.ToSlash(scaffold.AppFile))
	}
	main, err := mainSource(projectRoot)
	if err != nil {
		return authenticationPlan{}, err
	}
	if !strings.Contains(main, "bootstrap.Commands()") {
		return authenticationPlan{}, errors.New("cmd/<name>/main.go does not pass bootstrap.Commands() to forge.Main, so the identity migrations and the setup command would not be served; restore it (forge.Main(bootstrap.Configure, bootstrap.Commands()...)) and run the command again")
	}
	seam := openRoutesSeam(projectRoot, module, adapter, true)
	if !seam.wired {
		return authenticationPlan{}, errors.New("app/bootstrap/bootstrap.go does not call generatedRoutes (app/bootstrap/routes_gen.go), which mounts the sign-in pages; restore the call and run the command again")
	}
	features.Module, features.Adapter, features.Identity = module, adapter, true
	if plan.app, err = scaffold.RenderApp(features); err != nil {
		return authenticationPlan{}, err
	}
	if plan.routes, err = planIdentitySeam(projectRoot, seam); err != nil {
		return authenticationPlan{}, err
	}

	name := "app"
	if command, err := resolveCommand(root, nil); err == nil {
		if _, err := scaffold.Normalize(scaffold.Options{Name: command}); err == nil {
			name = command
		}
	}
	authentication, err := scaffold.AuthenticationFiles(scaffold.Options{Name: name, Module: module, Database: databaseOption(adapter), APIAuth: apiAuth})
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
	if err := rejectExistingSymlinkPaths(projectRoot, append(paths, ".env.local")); err != nil {
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
	local, err := projectRoot.ReadFile(".env.local")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return authenticationPlan{}, err
	}
	if !strings.Contains(string(local), "FORGE_AUTH_MFA_KEY=") {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return authenticationPlan{}, err
		}
		plan.mfaKey = "# App-specific 32-byte MFA encryption key (canonical unpadded base64url),\n# added by forge generate authentication.\nFORGE_AUTH_MFA_KEY=" + base64.RawURLEncoding.EncodeToString(key) + "\n"
		if len(local) > 0 && !strings.HasSuffix(string(local), "\n") {
			plan.mfaKey = "\n" + plan.mfaKey
		}
	}
	return plan, nil
}

// mainSource is the source of the application's one cmd/NAME/main.go.
func mainSource(projectRoot *os.Root) (string, error) {
	entries, err := os.ReadDir(filepath.Join(projectRoot.Name(), "cmd"))
	if err != nil {
		return "", errors.New("cmd/ not found: run this inside a Forge application")
	}
	var sources []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		source, err := projectRoot.ReadFile(filepath.Join("cmd", entry.Name(), "main.go"))
		if err == nil {
			sources = append(sources, string(source))
		}
	}
	if len(sources) != 1 {
		return "", fmt.Errorf("found %d commands with a main.go under cmd/, want the application's one", len(sources))
	}
	return sources[0], nil
}

// planIdentitySeam is routes_gen.go with the identity block, ahead of the
// blocks the generators wrote before.
func planIdentitySeam(projectRoot *os.Root, seam routesSeam) ([]byte, error) {
	if _, err := projectRoot.Lstat(routesTemporary); err == nil {
		return nil, fmt.Errorf("%s exists: an earlier write of the routes seam was interrupted. Check %s, then remove %s and run the command again", filepath.ToSlash(routesTemporary), filepath.ToSlash(scaffold.RoutesFile), filepath.ToSlash(routesTemporary))
	}
	if err := rejectExistingSymlinkPaths(projectRoot, []string{scaffold.RoutesFile}); err != nil {
		return nil, err
	}
	current, err := projectRoot.ReadFile(scaffold.RoutesFile)
	var blocks []scaffold.RouteBlock
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if blocks, err = scaffold.ParseRoutes(current); err != nil {
			return nil, err
		}
	}
	shared := make(map[string]string)
	var entries []scaffold.RouteBlock
	for _, block := range blocks {
		if block.Shared() {
			shared[block.ID()] = block.Body
		} else {
			entries = append(entries, block)
		}
	}
	seam.app.Identity = true
	return renderRoutes(seam, shared, entries, nil)
}

// writeAuthenticationWiring writes app_gen.go and the seam, and appends the
// MFA key to .env.local.
func writeAuthenticationWiring(root string, plan authenticationPlan) error {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = projectRoot.Close() }()
	if err := writeRootFileAtomic(projectRoot, scaffold.AppFile, plan.app); err != nil {
		return err
	}
	if err := writeRootFileAtomic(projectRoot, scaffold.RoutesFile, plan.routes); err != nil {
		return err
	}
	if plan.mfaKey == "" {
		return nil
	}
	file, err := projectRoot.OpenFile(".env.local", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(plan.mfaKey)
	return errors.Join(err, file.Close())
}

// authenticationNotes is printed after the files.
func authenticationNotes(plan authenticationPlan) string {
	var out strings.Builder
	if !plan.browserPipeline {
		out.WriteString("\n  WARNING: app/bootstrap/bootstrap.go does not appear to install csrf.Protection, web.MethodOverride and web.Flash (forge new generates useBrowserPipeline for them). auth.Browser refuses every form without a verified CSRF token, so sign-in cannot work until they are installed (docs/VIEWS.md).\n")
	}
	if !plan.i18nMiddleware {
		out.WriteString("\n  WARNING: app/bootstrap/bootstrap.go does not appear to build the translator and install its middleware (forge new does: `translator, err := locales.New(app.Config(), app.Logger())` in Configure, passed to generatedRoutes, and `translator.Middleware(i18n.NegotiationOptions{})` first in useBrowserPipeline). The pages' text comes from config/locales and needs them (docs/I18N.md).\n")
	}
	if len(plan.jsonAuthRoutes) > 0 && !plan.apiAuth {
		fmt.Fprintf(&out, "\n  NOTE: the application also mounts JSON authentication routes (%s). With the pages, browsers need none of them, and each is another place a password or code is entered. Remove those no API client or script uses.\n", strings.Join(plan.jsonAuthRoutes, ", "))
	}
	out.WriteString("\n  The sign-in pages are wired: routes_gen.go mounts them through identityRoutes (app/bootstrap/identity.go, yours to change), and app_gen.go adds the identity migrations and the setup command. Routes generated from now on need sign-in and their permissions; routes generated earlier keep their guard.\n")
	out.WriteString("\n  Then: forge migrate && forge setup --email you@example.com\n")
	return out.String()
}

// apiAuthOnlyWiring is printed for --api-auth when the application already
// has Forge identity: Forge writes nothing and prints the JSON API wiring.
func apiAuthOnlyWiring(plan authenticationPlan) string {
	var out strings.Builder
	out.WriteString("\nThis application has Forge identity already (app/bootstrap/identity.go, app/adapters/htmlauth); nothing was written.\n")
	if len(plan.jsonAuthRoutes) == len(jsonAuthRoutePatterns) {
		out.WriteString("\n  JSON authentication APIs (--api-auth): the application already mounts them; keep them.\n")
		return out.String()
	}
	if len(plan.jsonAuthRoutes) > 0 {
		fmt.Fprintf(&out, "\n  The application already mounts %s; add only the others below.\n", strings.Join(plan.jsonAuthRoutes, ", "))
	}
	out.WriteString("\nForge did not edit app/bootstrap/identity.go, which is yours. Mount the JSON authentication APIs there:\n")
	out.WriteString(apiAuthWiring)
	return out.String()
}

// jsonAuthRoutePatterns are the JSON authentication routes `forge new
// --api-auth` mounts (apiAuthRoutes in the generated identity.go).
var jsonAuthRoutePatterns = []string{
	"POST /auth/login", "POST /auth/logout", "POST /auth/mfa/complete",
	"POST /auth/recovery/request", "POST /auth/recovery/reset",
	"POST /auth/cookie/login", "POST /auth/cookie/mfa/complete", "POST /auth/cookie/logout",
	"GET /v1/auth/session",
}

// mountedJSONAuthRoutes lists the JSON authentication routes that
// app/bootstrap/bootstrap.go or identity.go register, as quoted route
// patterns.
func mountedJSONAuthRoutes(projectRoot *os.Root) []string {
	var source strings.Builder
	for _, name := range []string{"bootstrap.go", "identity.go"} {
		content, err := projectRoot.ReadFile(filepath.Join("app", "bootstrap", name))
		if err == nil {
			source.Write(content)
		}
	}
	var mounted []string
	for _, pattern := range jsonAuthRoutePatterns {
		if strings.Contains(source.String(), `"`+pattern+`"`) {
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
		return nil, nil, err
	}
	logoutHandler, err := auth.NewAuditedLogoutHandler(repository, repository)
	if err != nil {
		return nil, nil, err
	}
	if err := app.Handle("POST /auth/login", loginHandler); err != nil {
		return nil, nil, err
	}
	if err := app.Handle("POST /auth/logout", logoutHandler); err != nil {
		return nil, nil, err
	}
	cookieLoginHandler, err := auth.NewCookieLoginHandler(login) // browser scripts
	if err != nil {
		return nil, nil, err
	}
	cookieLogoutHandler, err := auth.NewAuditedCookieLogoutHandler(repository, repository)
	if err != nil {
		return nil, nil, err
	}
	if err := app.Handle("POST /auth/cookie/login", cookieLoginHandler); err != nil {
		return nil, nil, err
	}
	if err := app.Handle("POST /auth/cookie/logout", cookieLogoutHandler); err != nil {
		return nil, nil, err
	}

     With MFA, also mount auth.NewMFACompletionHandler(mfaService) at
     POST /auth/mfa/complete and auth.NewCookieMFACompletionHandler(mfaService)
     at POST /auth/cookie/mfa/complete; for JSON password recovery,
     auth.NewAuditedPasswordRecoveryRequestHandler(recoveryService, repository)
     and auth.NewAuditedPasswordResetHandler(recoveryService, repository) at
     POST /auth/recovery/request and /auth/recovery/reset. A new application
     shows the complete function: forge new NAME --api-auth (apiAuthRoutes in
     app/bootstrap/identity.go).
`
