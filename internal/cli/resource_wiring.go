package cli

import (
	"fmt"
	"strings"
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
	Usage    string // wires this resource
	Notes    []string
}

// buildResourceWiring is what `forge generate resource` and `forge generate
// scaffold` print: the JSON routes, and for a scaffold the HTML pages too.
func buildResourceWiring(spec resourceSpec) resourceWiringParts {
	parts := buildResourceRoutesWiring(spec)
	if spec.Scaffold {
		addScaffoldWiring(spec, &parts)
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
	if err := httpapi.Register%[3]s(app, %[1]sService, protect); err != nil {
		return err
	}
`, lowerCamel(spec.Name), title, plural),
	}
	if spec.Identity {
		parts.Where = "inside identityRoutes, after the `middleware, err := auth.NewMiddleware(repository)` statement and its error check"
		parts.Guard = `	protect := func(permission string, next http.Handler) (http.Handler, error) {
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
		parts.Notes = []string{
			fmt.Sprintf("The migration registers the permissions %s:read and %s:write; grant them to a role before callers can use the routes.", spec.Plural, spec.Plural),
		}
		if !spec.TenantScoped {
			// Roles belong to tenants, so a tenant administrator can grant
			// itself any permission. Writes to rows every tenant reads need
			// authorization above the tenant.
			parts.PlatformGuard = `	// Global rows are read by every tenant, but roles are per tenant: a tenant
	// administrator could grant itself the write permission and change data
	// that every other tenant sees. Writes therefore stay closed until you
	// implement platform-level (operator) authorization here.
	protectPlatformWrite := func(_ string, _ http.Handler) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			web.Error(w, r, auth.ErrPermissionDenied)
		}), nil
	}
`
			parts.Usage = fmt.Sprintf(`	%[1]sService, err := application.New%[2]sService(persistence.New%[2]sRepository(database), time.Now, uuid.New)
	if err != nil {
		return err
	}
	if err := httpapi.Register%[3]s(app, %[1]sService, func(permission string, next http.Handler) (http.Handler, error) {
		if permission == %[4]q {
			return protectPlatformWrite(permission, next)
		}
		return protect(permission, next)
	}); err != nil {
		return err
	}
`, lowerCamel(spec.Name), title, plural, spec.Plural+":write")
			parts.Notes = append(parts.Notes, fmt.Sprintf("%s is global: its read route uses %s:read, but its write routes stay denied until protectPlatformWrite authorizes platform operators.", spec.Plural, spec.Plural))
		}
		return parts
	}
	parts.Imports = append(parts.Imports, "github.com/rocketinsidecorp/forge-go/fault")
	parts.Where = "inside Configure, after the `routes(ctx, app)` call and its error check"
	tenantNote := ""
	if spec.TenantScoped {
		tenantNote = "\n\t// The resource is tenant-scoped: authenticate the caller and attach the\n\t// tenant with tenancy.WithContext, or every request fails with tenant_required."
	}
	parts.Guard = fmt.Sprintf(`	// TODO: replace this deny-all guard with your authentication and
	// authorization. It receives the permission each route needs (%q or
	// %q) and returns the handler to mount; the routes stay closed until you
	// implement it.%s
	protect := func(_ string, _ http.Handler) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			web.Error(w, r, fault.New("permission_denied", "permission denied", fault.CategoryForbidden, 0))
		}), nil
	}
`, spec.Plural+":read", spec.Plural+":write", tenantNote)
	parts.Notes = []string{
		"This application has no Forge identity, so the guard is yours to write; the generator never opens the routes to the public.",
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
	parts.Imports = append(parts.Imports, spec.Module+"/app/adapters/htmlui", spec.Module+"/app/views")
	parts.Renderer = `	renderer, err := views.New()
	if err != nil {
		return err
	}
`
	guard := "protect"
	if spec.Identity {
		guard = "protectCookie"
		parts.CookieGuard = `	protectCookie := func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		return middleware.AuthenticateCookie(guarded), nil
	}
`
		parts.Notes = append(parts.Notes, "The pages authenticate the session cookie that POST /auth/cookie/login sets; a browser without it receives the 401 JSON error, because Forge ships no login page. Their forms are protected by the CSRF middleware that bootstrap.go installs.")
	}
	if spec.Identity && !spec.TenantScoped {
		parts.Usage += fmt.Sprintf(`	if err := htmlui.Register%[1]s(app, %[2]sService, renderer, func(permission string, next http.Handler) (http.Handler, error) {
		if permission == %[3]q {
			return protectPlatformWrite(permission, next)
		}
		return protectCookie(permission, next)
	}); err != nil {
		return err
	}
`, plural, lowerCamel(spec.Name), spec.Plural+":write")
		parts.Notes = append(parts.Notes, fmt.Sprintf("The %s pages follow the JSON routes: reads use %s:read, while the new, edit, create, update and delete routes stay denied until protectPlatformWrite authorizes platform operators.", spec.Plural, spec.Plural))
	} else {
		parts.Usage += fmt.Sprintf(`	if err := htmlui.Register%[1]s(app, %[2]sService, renderer, %[3]s); err != nil {
		return err
	}
`, plural, lowerCamel(spec.Name), guard)
	}
	parts.Notes = append(parts.Notes, fmt.Sprintf("The %s pages are served at /%s and the JSON API at /v1/%s. New and edit forms need %s:write like the routes that save them.", title, spec.Plural, spec.Plural, spec.Plural))
	if !spec.BrowserPipeline {
		parts.Notes = append(parts.Notes, "WARNING: app/bootstrap/bootstrap.go does not appear to install csrf.Protection, web.MethodOverride and web.Flash (forge new generates useBrowserPipeline for them). Without them the forms are not CSRF-protected and update and delete do not work: add them before serving these pages (docs/VIEWS.md).")
	}
}

// resourceWiring renders the instructions printed after generation (and for
// --dry-run).
func resourceWiring(spec resourceSpec) string {
	parts := buildResourceWiring(spec)
	var out strings.Builder
	kind := "resource"
	if spec.Scaffold {
		kind = "scaffold"
	}
	fmt.Fprintf(&out, "\nForge did not edit app/bootstrap/bootstrap.go. Wire the %s there:\n\n", kind)
	out.WriteString("  1. Add these imports (skip any already present):\n\n")
	for _, path := range parts.Imports {
		fmt.Fprintf(&out, "\t%q\n", path)
	}
	fmt.Fprintf(&out, "\n  2. Once per application, %s, define the guard:\n\n%s", parts.Where, parts.Guard)
	if parts.PlatformGuard != "" {
		fmt.Fprintf(&out, "\n  This resource is global, so also define (once) the guard for its writes:\n\n%s", parts.PlatformGuard)
	}
	if parts.CookieGuard != "" {
		fmt.Fprintf(&out, "\n  The HTML pages authenticate the session cookie, so also define (once) their guard:\n\n%s", parts.CookieGuard)
	}
	if parts.Renderer != "" {
		fmt.Fprintf(&out, "\n  Build the view renderer the pages share (once):\n\n%s", parts.Renderer)
	}
	fmt.Fprintf(&out, "\n  3. For this resource, right after it:\n\n%s", parts.Usage)
	for _, note := range parts.Notes {
		fmt.Fprintf(&out, "\n  %s\n", note)
	}
	fmt.Fprintf(&out, "\n  Then: forge migrate && forge test\n")
	return out.String()
}

// lowerCamel converts snake_case to a lower-camel Go identifier
// (order_item becomes orderItem).
func lowerCamel(name string) string {
	title := exportedName(name)
	first := strings.SplitN(name, "_", 2)[0]
	return strings.ToLower(first) + title[len(first):]
}
