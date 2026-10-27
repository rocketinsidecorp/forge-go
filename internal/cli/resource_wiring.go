package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/rocketinsidecorp/forge-go/i18n"
)

// resourceWiringParts is the code the application owner adds to
// app/bootstrap/bootstrap.go. The generator prints it and never edits the
// file, so the line between generated and hand-written code stays visible.
type resourceWiringParts struct {
	Imports []string // import paths, unquoted
	Where   string   // human-readable insertion point
	Guard   string   // the protect function, defined once per application
	// PlatformGuard is the deny-all guard for writes to global resources in
	// applications with Forge identity; empty otherwise. Defined once.
	PlatformGuard string
	// CookieGuard is the guard for the HTML pages of a scaffold in
	// applications with Forge identity: the same permission check behind the
	// session cookie instead of a bearer token. Defined once.
	CookieGuard string
	// Renderer builds the view renderer a scaffold's pages share. Defined once.
	Renderer string
	// StreamSigner and StreamsRegister are the live-update wiring of
	// `--broadcast`, defined once per application: the signer must exist
	// before the renderer, the registration needs the guard's middleware.
	StreamSigner    string
	StreamsRegister string
	Usage           string // wires this resource
	Notes           []string
}

// identityJSONGuard is the guard of JSON routes (bearer token) in applications with Forge
// identity: the bearer token plus the permission. Defined once per application.
const identityJSONGuard = `	protect := func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		return middleware.Authenticate(guarded), nil
	}
`

// cookieJSONGuard is the guard of JSON routes in an application with Forge
// identity that issues no bearer tokens (created without --api-auth): the
// session cookie the sign-in page sets, the CSRF token in the X-CSRF-Token
// header of every change (the CSRF middleware lets JSON bodies through, so
// cookie-authenticated JSON routes check it themselves with
// auth.RequireCSRF), and the permission.
const cookieJSONGuard = `	// The JSON routes authenticate the session cookie the sign-in page sets:
	// this application issues no bearer tokens (forge new --api-auth adds
	// them). Changes must send the CSRF token in the X-CSRF-Token header
	// (the csrf-token meta tag of a page, or the __Host-forge_csrf cookie).
	protect := func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		checked, err := auth.RequireCSRF(guarded)
		if err != nil {
			return nil, err
		}
		return middleware.AuthenticateCookie(checked), nil
	}
`

// cookieJSONNote explains cookieJSONGuard.
const cookieJSONNote = "This application issues no bearer tokens (it was created without --api-auth), so the JSON routes authenticate the session cookie of the sign-in page, like the HTML pages: a signed-in browser (or a client holding the cookie) calls them, and every change sends the CSRF token in the X-CSRF-Token header. For API clients with bearer tokens, run `forge generate authentication --api-auth` and mount the routes it prints."

// hasBearerSignIn reports that app/bootstrap/bootstrap.go mounts the
// bearer-token login, POST /auth/login (forge new --api-auth, or the wiring
// of forge generate authentication --api-auth).
func hasBearerSignIn(projectRoot *os.Root) bool {
	return slices.Contains(mountedJSONAuthRoutes(projectRoot), "POST /auth/login")
}

// identityPagesGuard is the guard of HTML pages in applications with Forge
// identity: the session cookie plus the permission.
const identityPagesGuard = `	protectCookie := func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		// Signed-out browsers go to the sign-in page and come back; API
		// clients get the 401 JSON error.
		return middleware.AuthenticateBrowser("/session/new", guarded)
	}
`

// denyAllGuard is the guard of an application without Forge identity: it
// refuses everyone until the owner writes the real one. permissions lists the
// permissions the routes ask for, quoted, for the comment.
func denyAllGuard(permissions, tenantNote string) string {
	return fmt.Sprintf(`	// TODO: replace this deny-all guard with your authentication and
	// authorization. It receives the permission each route needs (%s) and
	// returns the handler to mount; the routes stay closed until you
	// implement it.%s
	protect := func(_ string, _ http.Handler) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			web.Error(w, r, fault.New("permission_denied", "permission denied", fault.CategoryForbidden, 0))
		}), nil
	}
`, permissions, tenantNote)
}

// The guards of the routes seam are package-level functions, so that an
// application declaring its own guard of the same name gets a compile error
// ("redeclared") rather than a silently shadowed one, until a generator runs
// again and leaves the seam's out (bootstrapDeclarations). Guards that need
// the session store take it as arguments: protect(repository, middleware).

// seamDenyAllGuard is denyAllGuard as the routes seam defines it, once for
// every generated resource and controller of the application.
const seamDenyAllGuard = `// protect refuses every request: this application has no Forge identity,
// so authenticating callers and checking the permission each route asks for
// ("products:read", "products:write", ...) are yours to write. Copy this
// function into another file of package bootstrap, change it there, and run
// a generator again: it then uses yours. Tenant-scoped routes also need the
// caller's tenant: attach it with tenancy.WithContext.
func protect(_ string, _ http.Handler) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		web.Error(w, r, fault.New("permission_denied", "permission denied", fault.CategoryForbidden, 0))
	}), nil
}
`

// publicGuard is the guard of the routes generated with --public in an
// application without Forge identity: it lets every request through.
const publicGuard = `// public lets every request through: the routes generated with --public are
// served to anyone who can reach the server, with no sign-in and no
// permission check. To put authentication in front, copy this function into
// another file of package bootstrap, change it there, and run a generator
// again: it then uses yours.
func public(_ string, next http.Handler) (http.Handler, error) {
	return next, nil
}
`

// seamCookieJSONGuard is cookieJSONGuard as the seam declares it.
const seamCookieJSONGuard = `// protect guards the JSON routes with the session cookie the sign-in page
// sets (this application issues no bearer tokens: forge new --api-auth adds
// them), the CSRF token in the X-CSRF-Token header of every change (the
// csrf-token meta tag of a page, or the __Host-forge_csrf cookie), and the
// permission.
func protect(repository auth.SecurityAuditor, middleware *auth.Middleware) func(string, http.Handler) (http.Handler, error) {
	return func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		checked, err := auth.RequireCSRF(guarded)
		if err != nil {
			return nil, err
		}
		return middleware.AuthenticateCookie(checked), nil
	}
}
`

// seamBearerJSONGuard is identityJSONGuard as the seam declares it.
const seamBearerJSONGuard = `// protect guards the JSON routes with the bearer token of POST /auth/login
// and the permission.
func protect(repository auth.SecurityAuditor, middleware *auth.Middleware) func(string, http.Handler) (http.Handler, error) {
	return func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		return middleware.Authenticate(guarded), nil
	}
}
`

// seamPagesGuard is identityPagesGuard as the seam declares it.
const seamPagesGuard = `// protectCookie guards the HTML pages with the session cookie and the
// permission. Signed-out browsers go to the sign-in page and come back; API
// clients get the 401 JSON error.
func protectCookie(repository auth.SecurityAuditor, middleware *auth.Middleware) func(string, http.Handler) (http.Handler, error) {
	return func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		return middleware.AuthenticateBrowser("/session/new", guarded)
	}
}
`

// platformWriteGuard is the deny-all guard of writes to global rows in an
// application with Forge identity.
const platformWriteGuard = `	// Global rows are read by every tenant, but roles are per tenant: a tenant
	// administrator could grant itself the write permission and change data
	// that every other tenant sees. Writes therefore stay closed until you
	// implement platform-level (operator) authorization here.
	protectPlatformWrite := func(_ string, _ http.Handler) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			web.Error(w, r, auth.ErrPermissionDenied)
		}), nil
	}
`

// seamPlatformWriteGuard is platformWriteGuard as the seam declares it.
const seamPlatformWriteGuard = `// protectPlatformWrite refuses writes to global rows: they are read by every
// tenant, but roles are per tenant, so a tenant administrator could grant
// itself the write permission and change data every other tenant sees.
// Copy this function into another file of package bootstrap to authorize
// platform operators there, and run a generator again: it then uses yours.
func protectPlatformWrite(_ string, _ http.Handler) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		web.Error(w, r, auth.ErrPermissionDenied)
	}), nil
}
`

// seamGuards maps the printed guards to their seam declarations.
var seamGuards = map[string]string{
	cookieJSONGuard:    seamCookieJSONGuard,
	identityJSONGuard:  seamBearerJSONGuard,
	identityPagesGuard: seamPagesGuard,
	platformWriteGuard: seamPlatformWriteGuard,
}

// guardReference is how wiring refers to the guard named name: in the seam
// of an application with Forge identity, protect and protectCookie are
// functions of the session store, called as protect(repository, middleware);
// a --public seam uses public in place of protect. The reference is built
// into the code as it is generated, never substituted into its text, so a
// resource or permission named like a guard cannot change it.
func guardReference(seam, identity, public bool, name string) string {
	switch {
	case seam && identity && (name == "protect" || name == "protectCookie"):
		return name + "(repository, middleware)"
	case seam && public && !identity && name == "protect":
		return "public"
	}
	return name
}

// guard is guardReference for this resource.
func (spec resourceSpec) guard(name string) string {
	return guardReference(spec.Seam, spec.Identity, spec.Public, name)
}

// toSeam turns the printed guards into the seam's package-level guard
// functions. The usage already refers to them (guardReference).
func toSeam(parts *resourceWiringParts) {
	for _, guard := range []*string{&parts.Guard, &parts.PlatformGuard, &parts.CookieGuard} {
		if declared, ok := seamGuards[*guard]; ok {
			*guard = declared
		}
	}
}

// publicNote is the warning printed for --public routes. On PostgreSQL the
// application is API-only (forge new --api): Forge identity exists there but
// nothing signs callers in.
func publicNote(database string) string {
	why := "this application has no Forge identity, whose sign-in pages and `forge setup` are PostgreSQL-only"
	if database == "postgres" {
		why = "this API-only application (forge new --api) signs nobody in: it has no sign-in pages, `forge setup` or bearer login"
	}
	return "WARNING: --public serves these routes to anyone who can reach the server, with no sign-in and no permission check (" + why + "). CSRF protection still applies to the forms of full-stack applications."
}

// buildResourceWiring is what `forge generate resource` and `forge generate
// scaffold` print: the JSON routes, and for a scaffold the HTML pages too.
func buildResourceWiring(spec resourceSpec) resourceWiringParts {
	parts := buildResourceRoutesWiring(spec)
	if spec.Scaffold {
		addScaffoldWiring(spec, &parts)
	}
	if spec.Seam {
		toSeam(&parts)
	}
	return parts
}

func buildResourceRoutesWiring(spec resourceSpec) resourceWiringParts {
	title := exportedName(spec.Name)
	plural := exportedName(spec.Plural)
	parts := resourceWiringParts{
		Imports: []string{
			"time",
			spec.Module + "/app/adapters/httpapi",
			spec.Module + "/app/adapters/persistence",
			spec.Module + "/app/application",
			"github.com/rocketinsidecorp/forge-go/uuid",
		},
		Usage: fmt.Sprintf(`	%[1]sService, err := application.New%[2]sService(persistence.New%[2]sRepository(database), time.Now, uuid.New)
	if err != nil {
		return err
	}
	if err := httpapi.Register%[3]s(app, %[1]sService, %[4]s); err != nil {
		return err
	}
`, lowerCamel(spec.Name), title, plural, spec.guard("protect")),
	}
	if spec.Identity {
		parts.Where = "inside identityRoutes, after the `middleware, err := auth.NewMiddleware(repository)` statement and its error check"
		parts.Guard = identityJSONGuard
		parts.Notes = []string{
			fmt.Sprintf("The migration registers the permissions %s:read and %s:write and grants them to the owner role (`forge setup` creates it); grant them to other roles before their users can use the routes.", spec.Plural, spec.Plural),
		}
		if !spec.BearerSignIn {
			parts.Guard = cookieJSONGuard
			parts.Notes = append(parts.Notes, cookieJSONNote)
		}
		if spec.Organizations && !spec.TenantScoped {
			// Roles belong to tenants, so a tenant administrator can grant
			// itself any permission. Writes to rows every tenant reads need
			// authorization above the tenant.
			parts.PlatformGuard = platformWriteGuard
			parts.Usage = fmt.Sprintf(`	%[1]sService, err := application.New%[2]sService(persistence.New%[2]sRepository(database), time.Now, uuid.New)
	if err != nil {
		return err
	}
	if err := httpapi.Register%[3]s(app, %[1]sService, func(permission string, next http.Handler) (http.Handler, error) {
		if permission == %[4]q {
			return protectPlatformWrite(permission, next)
		}
		return %[5]s(permission, next)
	}); err != nil {
		return err
	}
`, lowerCamel(spec.Name), title, plural, spec.Plural+":write", spec.guard("protect"))
			parts.Notes = append(parts.Notes, fmt.Sprintf("%s is global: its read route uses %s:read, but its write routes stay denied until protectPlatformWrite authorizes platform operators.", spec.Plural, spec.Plural))
		}
		return parts
	}
	// The deny-all guard answers with web.Error; an API-only application's
	// bootstrap.go (forge new --api) does not import web yet.
	parts.Imports = append(parts.Imports, "github.com/rocketinsidecorp/forge-go/fault", "github.com/rocketinsidecorp/forge-go/web")
	parts.Where = "inside Configure, after the `routes(ctx, app)` call and its error check"
	tenantNote := ""
	if spec.TenantScoped {
		tenantNote = "\n\t// The resource is tenant-scoped: authenticate the caller and attach the\n\t// tenant with tenancy.WithContext, or every request fails with tenant_required."
	}
	parts.Guard = denyAllGuard(fmt.Sprintf("%q or %q", spec.Plural+":read", spec.Plural+":write"), tenantNote)
	parts.Notes = []string{
		"This application has no Forge identity, so the guard is yours to write; the generator never opens the routes to the public.",
	}
	if spec.Seam {
		parts.Guard = seamDenyAllGuard
		parts.Notes = []string{
			"This application has no Forge identity, so the routes are mounted closed: protect refuses every request until you write it (copy func protect from app/bootstrap/routes_gen.go into another file of package bootstrap, change it, and run a generator again). To serve them without sign-in instead, generate them with --public (global resources only).",
		}
		if spec.Public {
			parts.Guard = publicGuard
			parts.Notes = []string{publicNote(spec.Database)}
		}
	}
	return parts
}

// addScaffoldWiring adds the HTML pages to the wiring of a resource. They are
// guarded like the JSON routes: the same permissions (reads for index and
// show, writes for the forms and every change), and in applications with
// Forge identity the session cookie replaces the bearer token. Global
// resources keep their writes behind the platform guard.
func addScaffoldWiring(spec resourceSpec, parts *resourceWiringParts) {
	title := exportedName(spec.Name)
	plural := exportedName(spec.Plural)
	parts.Imports = append(parts.Imports, spec.Module+"/app/adapters/htmlui")
	rendererCall := "views.New(views.WithI18n(translator))"
	if spec.Broadcast {
		rendererCall = "views.New(views.WithI18n(translator), views.WithStreams(streamSigner))"
	}
	if spec.ViewRenderer {
		if spec.Broadcast {
			parts.Notes = append(parts.Notes, "bootstrap.go already builds the renderer with views.New(views.WithI18n(translator)): define streamSigner (below) before that line and change it to `"+rendererCall+"`, so the pages can sign their stream names.")
		} else {
			parts.Notes = append(parts.Notes, "The pages reuse the renderer that bootstrap.go already builds with views.New(views.WithI18n(translator)).")
		}
	} else {
		parts.Imports = append(parts.Imports, spec.Module+"/app/views")
		parts.Renderer = "	renderer, err := " + rendererCall + `
	if err != nil {
		return err
	}
`
	}
	if spec.Broadcast {
		addBroadcastWiring(spec, parts)
	}
	guard := spec.guard("protect")
	if spec.Identity {
		guard = spec.guard("protectCookie")
		parts.CookieGuard = identityPagesGuard
		parts.Notes = append(parts.Notes, "The pages authenticate the session cookie the sign-in page (/session/new) sets (as does POST /auth/cookie/login in applications created with --api-auth); a signed-out browser is redirected to /session/new and back, while API clients get the 401 JSON error. forge new generates the sign-in page; run `forge generate authentication` in an application that lacks it. Their forms are protected by the CSRF middleware that bootstrap.go installs.")
	}
	if spec.Identity && spec.Organizations && !spec.TenantScoped {
		parts.Usage += fmt.Sprintf(`	if err := htmlui.Register%[1]s(app, %[2]sService, renderer, func(permission string, next http.Handler) (http.Handler, error) {
		if permission == %[3]q {
			return protectPlatformWrite(permission, next)
		}
		return %[4]s(permission, next)
	}); err != nil {
		return err
	}
`, plural, lowerCamel(spec.Name), spec.Plural+":write", spec.guard("protectCookie"))
		parts.Notes = append(parts.Notes, fmt.Sprintf("The %s pages follow the JSON routes: reads use %s:read, while the new, edit, create, update and delete routes stay denied until protectPlatformWrite authorizes platform operators.", spec.Plural, spec.Plural))
	} else {
		parts.Usage += fmt.Sprintf(`	if err := htmlui.Register%[1]s(app, %[2]sService, renderer, %[3]s); err != nil {
		return err
	}
`, plural, lowerCamel(spec.Name), guard)
	}
	if spec.Broadcast {
		parts.Usage = addBroadcastOption(parts.Usage, plural, title)
	}
	parts.Notes = append(parts.Notes, fmt.Sprintf("The %s pages are served at /%s and the JSON API at /v1/%s. New and edit forms need %s:write like the routes that save them.", title, spec.Plural, spec.Plural, spec.Plural))
	if !spec.BrowserPipeline {
		parts.Notes = append(parts.Notes, "WARNING: app/bootstrap/bootstrap.go does not appear to install csrf.Protection, web.MethodOverride and web.Flash (forge new generates useBrowserPipeline for them). Without them the forms are not CSRF-protected and update and delete do not work: add them before serving these pages (docs/VIEWS.md).")
	}
	files := fmt.Sprintf("config/locales/%s.en.json (English)", spec.Plural)
	if spec.DefaultLocale != "" && spec.DefaultLocale != i18n.DefaultLocale {
		files = fmt.Sprintf("config/locales/%[1]s.en.json and, as the starting point for the default locale, config/locales/%[1]s.%[2]s.json (both English: translate the second)", spec.Plural, spec.DefaultLocale)
	}
	parts.Notes = append(parts.Notes, fmt.Sprintf("The pages' text is in %s. Translate it into %s.LOCALE.json for every other locale in config/locales; `forge i18n check` lists what is missing.", files, spec.Plural))
	if !spec.I18nMiddleware {
		parts.Notes = append(parts.Notes, "WARNING: app/bootstrap/bootstrap.go does not appear to build the translator and install its middleware (forge new does: `translator, err := locales.New(app.Config(), app.Logger())` in Configure, and `translator.Middleware(i18n.NegotiationOptions{})` first in useBrowserPipeline). The pages' t helper fails without them (docs/I18N.md).")
	}
}

// resourceWiring renders the instructions printed after generation (and for
// --dry-run).
func resourceWiring(spec resourceSpec) string {
	kind := "resource"
	if spec.Scaffold {
		kind = "scaffold"
	}
	return formatWiring(kind, "resource", buildResourceWiring(spec), "forge migrate && forge test")
}

// formatWiring prints parts as numbered steps. kind names what is wired
// ("resource", "scaffold", "controller"), subject what step 3 is for and then
// the command to run afterwards.
func formatWiring(kind, subject string, parts resourceWiringParts, then string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "\nForge did not edit app/bootstrap/bootstrap.go. Wire the %s there:\n\n", kind)
	out.WriteString("  1. Add these imports (skip any already present):\n\n")
	for _, path := range parts.Imports {
		fmt.Fprintf(&out, "\t%q\n", path)
	}
	if parts.Guard != "" {
		fmt.Fprintf(&out, "\n  2. Once per application, %s, define the guard:\n\n%s", parts.Where, parts.Guard)
	} else {
		fmt.Fprintf(&out, "\n  2. %s:\n", capitalize(parts.Where))
	}
	if parts.PlatformGuard != "" {
		fmt.Fprintf(&out, "\n  This resource is global, so also define (once) the guard for its writes:\n\n%s", parts.PlatformGuard)
	}
	if parts.CookieGuard != "" {
		fmt.Fprintf(&out, "\n  The HTML pages authenticate the session cookie, so also define (once) their guard:\n\n%s", parts.CookieGuard)
	}
	if parts.StreamSigner != "" {
		fmt.Fprintf(&out, "\n  Live updates (--broadcast), define once, before the view renderer:\n\n%s", parts.StreamSigner)
	}
	if parts.Renderer != "" {
		fmt.Fprintf(&out, "\n  Build the view renderer the pages share (once):\n\n%s", parts.Renderer)
	}
	if parts.StreamsRegister != "" {
		fmt.Fprintf(&out, "\n  Then mount the event stream (once, after the guard above):\n\n%s", parts.StreamsRegister)
	}
	fmt.Fprintf(&out, "\n  3. For this %s, right after it:\n\n%s", subject, parts.Usage)
	for _, note := range parts.Notes {
		fmt.Fprintf(&out, "\n  %s\n", note)
	}
	fmt.Fprintf(&out, "\n  Then: %s\n", then)
	return out.String()
}

// lowerCamel converts snake_case to a lower-camel Go identifier
// (order_item becomes orderItem).
func lowerCamel(name string) string {
	title := exportedName(name)
	first := strings.SplitN(name, "_", 2)[0]
	return strings.ToLower(first) + title[len(first):]
}

// addBroadcastOption appends the broker option to the Register<Plural> call,
// whose guard argument is a name or a function literal.
func addBroadcastOption(usage, plural, title string) string {
	option := "htmlui.With" + title + "Broadcast(streams.Broker)"
	call := "htmlui.Register" + plural + "("
	start := strings.Index(usage, call)
	if start < 0 {
		return usage
	}
	end := start + strings.Index(usage[start:], "); err != nil {")
	if end < start {
		return usage
	}
	return usage[:end] + ", " + option + usage[end:]
}

// addBroadcastWiring prints the once-per-application live-update wiring.
func addBroadcastWiring(spec resourceSpec, parts *resourceWiringParts) {
	parts.Imports = append(parts.Imports,
		"os",
		"github.com/rocketinsidecorp/forge-go/broadcast",
		spec.Module+"/app/adapters/htmlstreams",
	)
	parts.StreamSigner = `	streamSigner, err := htmlstreams.NewSigner(app.Config().Environment, os.Getenv(htmlstreams.KeyEnvironment), os.Getenv(htmlstreams.PreviousKeysEnvironment), app.Logger())
	if err != nil {
		return err
	}
`
	if spec.Identity {
		parts.StreamsRegister = `	streams, err := htmlstreams.Register(ctx, app, streamSigner, htmlstreams.Options{
		Authenticate: middleware.AuthenticateCookie,
		// A channel is read with the permission of the resource it carries:
		// the "posts" channel needs posts:read. Tenant channels are already
		// limited to the caller's tenant.
		Authorize: func(r *http.Request, channel broadcast.Channel) error {
			principal, ok := auth.FromContext(r.Context())
			if !ok {
				return auth.ErrCredentialsRequired
			}
			required, err := auth.NewPermission(channel.Name + ":read")
			if err != nil || !principal.Can(required) {
				return auth.ErrPermissionDenied
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
`
		if !spec.Seam {
			parts.Notes = append(parts.Notes, "Live updates: ctx must be in scope in identityRoutes (forge new passes it: identityRoutes(ctx, app, database)). Define streamSigner and streams once, in this order: streamSigner before the view renderer, streams after `middleware, err := auth.NewMiddleware(repository)`.")
		}
	} else {
		parts.Imports = append(parts.Imports, "github.com/rocketinsidecorp/forge-go/fault", "github.com/rocketinsidecorp/forge-go/web")
		parts.StreamsRegister = `	// This application has no Forge identity, so authenticating the caller
	// (attach the principal and tenant to the request context) and deciding
	// who may read each channel are yours to write. Until then both refuse
	// everyone: live updates stay closed rather than public.
	denyStreams := fault.New("permission_denied", "permission denied", fault.CategoryForbidden, 0)
	streams, err := htmlstreams.Register(ctx, app, streamSigner, htmlstreams.Options{
		Authenticate: func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { web.Error(w, r, denyStreams) })
		},
		Authorize: func(*http.Request, broadcast.Channel) error { return denyStreams },
	})
	if err != nil {
		return err
	}
`
		if spec.Seam {
			parts.Notes = append(parts.Notes, "Live updates stay closed in this application without Forge identity: the event stream refuses everyone until you authenticate its readers (the streams block of app/bootstrap/routes_gen.go shows what to write).")
		} else {
			parts.Notes = append(parts.Notes, "Live updates: define streamSigner and streams once, streamSigner before the view renderer and streams before the pages that publish.")
		}
	}
	parts.Notes = append(parts.Notes,
		"Set FORGE_STREAMS_KEY to `openssl rand -base64 32` (the same value on every replica) before running in staging or production; development and tests fall back to a random per-process key. To rotate it without breaking open pages, redeploy twice: first FORGE_STREAMS_KEY=old with FORGE_STREAMS_PREVIOUS_KEYS=new, then the two swapped (previous keys, comma-separated, at most 3, only verify); unset FORGE_STREAMS_PREVIOUS_KEYS after the token lifetime (24 hours by default), see docs/VIEWS.md. The in-memory broker serves one process: for several replicas, build the broker with forge-go's broadcast/postgres package (docs/VIEWS.md).",
	)
}
