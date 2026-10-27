package cli

import (
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

const resourceUsage = "usage: forge generate resource NAME field:type... [--org-scoped | --global] [--plural PLURAL] [--public] [--dry-run]"

const (
	maxResourceFields     = 40
	maxResourceNameLength = 40
)

var resourceName = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z][a-z0-9]*)*$`)

// resourceFieldTypes are the only column types a generated resource supports.
// Every one has an exact, portable representation on all four databases:
// there is deliberately no floating-point or decimal type (money is an int in
// minor units).
var resourceFieldTypes = []string{"string", "text", "int", "bool", "time", "uuid"}

// fieldTypeAliases are the other spellings a field type is accepted under
// (decision D22: Rails's integer, boolean, timestamp and datetime).
var fieldTypeAliases = map[string]string{"integer": "int", "boolean": "bool", "timestamp": "time", "datetime": "time"}

// reservedColumns are the columns every generated table owns.
// tenant_id stays reserved beside org_id: it is the framework's name for the
// organization (forge_tenants), and a column so named would read as one.
var reservedColumns = []string{"id", "org_id", "tenant_id", "version", "created_at", "updated_at"}

// fileNameSuffixes are the segments that make Go treat a file name as a build
// constraint (name_linux.go, name_test.go), which would silently exclude the
// generated file from some builds or make it a test file.
var fileNameSuffixes = []string{
	"test",
	"aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "js", "linux", "nacl", "netbsd",
	"openbsd", "plan9", "solaris", "wasip1", "windows", "zos",
	"amd64", "arm", "arm64", "loong64", "mips", "mipsle", "mips64", "mips64le", "ppc64", "ppc64le", "riscv64", "s390x",
	"sparc64", "wasm",
}

// reservedGoMembers are the exported fields and methods of the generated
// entity and changes types (ID, Version, CreatedAt, UpdatedAt, Apply and
// IsEmpty); a user field that exports to one of them would not compile. A
// test derives the methods from the rendered code, so the list cannot drift
// from the templates.
var reservedGoMembers = []string{"ID", "Version", "CreatedAt", "UpdatedAt", "Apply", "IsEmpty"}

// goReservedNames are Go keywords, predeclared identifiers and the packages
// the generated files import. Generated code never uses a resource name or
// plural as a bare identifier, but rejecting them keeps the output readable
// and safe against future templates.
var goReservedNames = strings.Fields(`
	break case chan const continue default defer else fallthrough for func go goto if import interface map package
	range return select struct switch type var
	any bool byte comparable complex128 complex64 error float32 float64 int int16 int32 int64 int8 rune string uint
	uint16 uint32 uint64 uint8 uintptr true false iota nil append cap clear close complex copy delete imag len make max
	min new panic print println real recover
	bytes cmp context errors fmt http json maps reflect slices sql strconv strings sync testing time
	application domain httpapi persistence db fault forge pagination pgx postgres mysql sqlite sqldb tenancy uuid web`)

// resourceRequest is a syntactically and semantically valid command line.
type resourceRequest struct {
	Name         string
	Plural       string
	Fields       []resourceField
	TenantScoped bool
	// Global is --global: rows shared by every organization of an
	// application with organizations.
	Global bool
	DryRun bool
	// Scaffold is set by `forge generate scaffold`: the resource plus its
	// server-rendered HTML pages. It changes only what is generated around
	// the resource, never the resource itself.
	Scaffold bool
	// Broadcast (`forge generate scaffold --broadcast`) makes the scaffold's
	// pages publish created, updated and deleted rows to open index pages as
	// Turbo Streams over Server-Sent Events.
	Broadcast bool
	// Public (`--public`) serves the routes to anyone, in an application
	// without Forge identity, where the generated guard otherwise refuses
	// every request. It needs --global: a tenant-scoped resource needs the
	// caller's tenant, which an anonymous request does not carry.
	Public bool
	// Model is set by `forge generate model`: the resource without its HTTP
	// layer (migration, domain, application and persistence only).
	Model bool
	// Destroy plans for `forge destroy`: the same files, found where they
	// are, with no refusal for files that already exist.
	Destroy bool
	// organizations renders the resource for an application with
	// organizations whatever app_gen.go says: `forge generate tenancy`
	// renders the converted code before it flips the application.
	organizations bool
}

// resourceField is one user-declared column.
type resourceField struct {
	Name     string // snake_case column and JSON name
	Type     string // one of resourceFieldTypes
	Optional bool
}

// resourceSpec is a request resolved against an application.
type resourceSpec struct {
	resourceRequest
	Module   string
	Database string // postgres, mysql (also MariaDB) or sqlite
	Identity bool   // Forge identity (permissions table) is available
	// Organizations reports an application with organizations (forge new
	// --tenants): tables need a scope, and global rows are written only by
	// platform operators. Without them every generated table is a plain
	// table of the application's one organization.
	Organizations bool
	// BearerSignIn reports that bootstrap.go mounts the bearer-token login
	// (POST /auth/login, from --api-auth): without it no caller can hold the
	// bearer token the JSON routes of an identity application authenticate,
	// so they are wired closed.
	BearerSignIn bool
	// BrowserPipeline reports that app/bootstrap/bootstrap.go appears to
	// install the CSRF, method-override and flash middleware server-rendered
	// forms rely on. Only scaffolds look.
	BrowserPipeline bool
	// I18nMiddleware reports that bootstrap.go appears to build the
	// translator and install its middleware.
	I18nMiddleware bool
	// DefaultLocale is the application's default locale (see
	// appDefaultLocale). Only scaffolds look.
	DefaultLocale string
	// ViewRenderer reports that bootstrap.go already builds the view
	// renderer (renderer, err := views.New()), as PostgreSQL applications
	// from `forge new` do for the sign-in pages. Only scaffolds look.
	ViewRenderer bool
	// AddsViewBase reports that the scaffold also adds the views base
	// (layout, asset bundle, JavaScript) because the application has none.
	AddsViewBase bool
	// AddsStreams reports that the scaffold also adds app/adapters/htmlstreams
	// (the first --broadcast scaffold of the application).
	AddsStreams bool
	// Seam reports that bootstrap.go calls the generated routes seam
	// (app/bootstrap/routes_gen.go): the generator wires the routes there
	// instead of printing them.
	Seam bool
	// seam is the application's seam when Seam is set.
	seam routesSeam
}

func runGenerateResource(args []string, stdout io.Writer, root string, env []string, now time.Time) error {
	request, err := parseResourceArgs(args)
	if err != nil {
		return err
	}
	if request.Broadcast {
		return fmt.Errorf("--broadcast is only for `forge generate scaffold`: a JSON resource has no pages to update\n%s", resourceUsage)
	}
	spec, files, err := planResource(root, request, env, now)
	if err != nil {
		return err
	}
	return generateResourceFiles(stdout, root, spec, files, []string{"db/migrations", "app/domain", "app/application", "app/adapters/persistence", "app/adapters/httpapi"})
}

// generateResourceFiles writes the planned files of a resource or scaffold
// (unless --dry-run), wires the routes into the seam when the application has
// one, and reports both. The seam is planned before anything is written, so a
// refused seam writes nothing.
func generateResourceFiles(stdout io.Writer, root string, spec resourceSpec, files []generatedFile, directories []string) error {
	var wiring seamWiring
	if spec.Seam {
		var err error
		if wiring, err = planResourceSeam(root, spec, false); err != nil {
			return err
		}
	}
	if !spec.DryRun {
		if err := applyGeneratedFiles(root, directories, newFiles(files)); err != nil {
			return err
		}
		if spec.Seam && wiring.verb == "wire" {
			if err := writeRoutes(root, wiring.content); err != nil {
				return err
			}
		}
	}
	return reportGenerated(stdout, spec, files, wiring)
}

// planResourceSeam plans the seam with the resource's routes added (or, for
// `forge destroy`, removed).
func planResourceSeam(root string, spec resourceSpec, remove bool) (seamWiring, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return seamWiring{}, err
	}
	defer func() { _ = projectRoot.Close() }()
	parts := buildResourceWiring(spec)
	kind := "resource"
	if spec.Scaffold {
		kind = "scaffold"
	}
	content, verb, err := planRoutes(projectRoot, spec.seam, routesChange{
		entry:  scaffold.RouteBlock{Kind: kind, Name: spec.Plural, Body: parts.Usage},
		shared: sharedFromParts(parts),
		remove: remove,
	})
	return seamWiring{content: content, verb: verb, notes: parts.Notes}, err
}

// reportGenerated lists the files (planned, for --dry-run) and the seam, or
// prints the wiring the application owner adds to bootstrap.go when the
// application has no seam.
func reportGenerated(stdout io.Writer, spec resourceSpec, files []generatedFile, wiring seamWiring) error {
	verb := "create"
	if spec.DryRun {
		verb = "would create"
	}
	for _, file := range files {
		fileVerb := verb
		if file.identical {
			fileVerb = "identical"
		}
		if _, err := fmt.Fprintf(stdout, "  %s  %s\n", fileVerb, file.path); err != nil {
			return err
		}
	}
	if !spec.Seam {
		_, err := io.WriteString(stdout, resourceWiring(spec))
		return err
	}
	_, err := io.WriteString(stdout, reportSeam(wiring, spec.DryRun, "forge migrate && forge test"))
	return err
}

// parseResourceArgs reads `NAME field:type... [--org-scoped|--global]
// [--plural P] [--dry-run]`. Flags may appear anywhere. It touches no files.
func parseResourceArgs(args []string) (resourceRequest, error) {
	return parseGeneratorArgs(args, resourceUsage)
}

// parseGeneratorArgs is parseResourceArgs for any generator that takes the
// resource command line; usage is what a malformed one is answered with.
func parseGeneratorArgs(args []string, usage string) (resourceRequest, error) {
	var request resourceRequest
	var tenantScoped, global, plural bool
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--dry-run":
			if request.DryRun {
				return resourceRequest{}, errors.New("--dry-run given more than once")
			}
			request.DryRun = true
		case arg == "--broadcast":
			if request.Broadcast {
				return resourceRequest{}, errors.New("--broadcast given more than once")
			}
			request.Broadcast = true
		case arg == "--public":
			if request.Public {
				return resourceRequest{}, errors.New("--public given more than once")
			}
			request.Public = true
		case arg == "--org-scoped" || arg == "--tenant-scoped":
			tenantScoped = true
		case arg == "--global":
			global = true
		case arg == "--plural" || strings.HasPrefix(arg, "--plural="):
			if plural {
				return resourceRequest{}, errors.New("--plural given more than once")
			}
			plural = true
			if value, ok := strings.CutPrefix(arg, "--plural="); ok {
				request.Plural = value
			} else if i+1 < len(args) {
				i++
				request.Plural = args[i]
			}
			if request.Plural == "" {
				return resourceRequest{}, errors.New("--plural needs a value")
			}
		case strings.HasPrefix(arg, "-"):
			return resourceRequest{}, usageError{fmt.Sprintf("unknown flag %q\n%s", arg, usage)}
		default:
			positional = append(positional, arg)
		}
	}
	if tenantScoped && global {
		return resourceRequest{}, errors.New("--org-scoped and --global are mutually exclusive: choose exactly one")
	}
	request.TenantScoped, request.Global = tenantScoped, global
	if request.Public && tenantScoped {
		return resourceRequest{}, errors.New("--public serves the routes without sign-in, and an org-scoped resource needs the caller's organization, which an anonymous request does not carry: drop --org-scoped (or use --global) with --public")
	}
	if len(positional) == 0 {
		return resourceRequest{}, usageError{usage}
	}
	request.Name = positional[0]
	if err := validateResourceName(request.Name); err != nil {
		return resourceRequest{}, err
	}
	if !plural {
		if singular, ok := looksPlural(request.Name); ok {
			return resourceRequest{}, fmt.Errorf("resource name %q looks plural: name the resource in the singular (%q) or pass --plural to set the plural explicitly", request.Name, singular)
		}
		request.Plural = pluralize(request.Name)
	}
	if err := validateResourcePlural(request.Name, request.Plural, plural); err != nil {
		return resourceRequest{}, err
	}
	if len(positional) == 1 {
		return resourceRequest{}, errors.New("declare at least one field, for example: title:string")
	}
	if len(positional)-1 > maxResourceFields {
		return resourceRequest{}, fmt.Errorf("a resource has at most %d fields", maxResourceFields)
	}
	seen := make(map[string]bool)
	seenGo := make(map[string]bool, len(reservedGoMembers))
	for _, member := range reservedGoMembers {
		seenGo[member] = true
	}
	for _, declaration := range positional[1:] {
		field, err := parseResourceField(declaration)
		if err != nil {
			return resourceRequest{}, err
		}
		if seen[field.Name] {
			return resourceRequest{}, fmt.Errorf("field %q is declared twice", field.Name)
		}
		seen[field.Name] = true
		title := exportedName(field.Name)
		if seenGo[title] {
			return resourceRequest{}, fmt.Errorf("field %q collides with a generated Go field (%s); rename it", field.Name, title)
		}
		seenGo[title] = true
		request.Fields = append(request.Fields, field)
	}
	for _, field := range request.Fields {
		if field.Optional && seen["clear_"+field.Name] {
			return resourceRequest{}, fmt.Errorf("field %q collides with the generated Clear%s flag of optional field %q; rename it", "clear_"+field.Name, exportedName(field.Name), field.Name)
		}
	}
	if err := validateIdentifierLengths(request); err != nil {
		return resourceRequest{}, err
	}
	return request, nil
}

func validateResourceName(name string) error {
	if err := rejectSeamReservedName("resource name", name); err != nil {
		return err
	}
	switch {
	case len(name) > maxResourceNameLength || !resourceName.MatchString(name):
		return fmt.Errorf("resource name %q must be singular snake_case (lowercase letters and digits, words separated by single underscores, starting with a letter, at most %d characters)", name, maxResourceNameLength)
	case slices.Contains(goReservedNames, name):
		return fmt.Errorf("resource name %q is a Go keyword, predeclared identifier or a package the generated code imports; choose another name", name)
	case hasFileNameSuffix(name):
		return fmt.Errorf("resource name %q ends in a word Go treats as a file-name build constraint (such as _test or _linux); choose another name", name)
	}
	return nil
}

func validateResourcePlural(name, plural string, explicit bool) error {
	if err := rejectSeamReservedName("plural", plural); err != nil {
		return err
	}
	hint := ""
	if !explicit {
		hint = "; pass --plural"
	}
	switch {
	case len(plural) > maxResourceNameLength || !resourceName.MatchString(plural):
		return fmt.Errorf("plural %q must be snake_case with at most %d characters%s", plural, maxResourceNameLength, hint)
	case slices.Contains(goReservedNames, plural):
		return fmt.Errorf("plural %q is a Go keyword, predeclared identifier or a package the generated code imports; pass another --plural", plural)
	case plural == name:
		return fmt.Errorf("plural %q must differ from the resource name; pass --plural", plural)
	case hasFileNameSuffix(plural):
		return fmt.Errorf("plural %q ends in a word Go treats as a file-name build constraint; pass another --plural", plural)
	case strings.HasPrefix(plural, "forge_") || strings.HasPrefix(plural, "sqlite_"):
		return fmt.Errorf("plural %q uses a table-name prefix reserved for Forge and SQLite internals", plural)
	}
	return nil
}

func parseResourceField(declaration string) (resourceField, error) {
	return parseField(declaration, true)
}

// parseField reads name:type or name:type?. Table columns may not reuse the
// columns every generated table owns; other uses of fields (a job payload)
// pass reserveColumns false.
func parseField(declaration string, reserveColumns bool) (resourceField, error) {
	name, kind, ok := strings.Cut(declaration, ":")
	if !ok {
		return resourceField{}, fmt.Errorf("field %q must be written name:type (types: %s)", declaration, strings.Join(resourceFieldTypes, ", "))
	}
	optional := false
	if trimmed, found := strings.CutSuffix(kind, "?"); found {
		kind, optional = trimmed, true
	}
	if canonical, ok := fieldTypeAliases[kind]; ok {
		kind = canonical
	}
	switch {
	case len(name) > maxResourceNameLength || !resourceName.MatchString(name):
		return resourceField{}, fmt.Errorf("field name %q must be snake_case (lowercase letters and digits, starting with a letter, at most %d characters)", name, maxResourceNameLength)
	case reserveColumns && slices.Contains(reservedColumns, name):
		return resourceField{}, fmt.Errorf("field name %q is reserved: every table already has %s", name, strings.Join(reservedColumns, ", "))
	case !slices.Contains(resourceFieldTypes, kind):
		return resourceField{}, fmt.Errorf("field %q has unsupported type %q (types: %s; use int for money in minor units)", name, kind, strings.Join(resourceFieldTypes, ", "))
	}
	return resourceField{Name: name, Type: kind, Optional: optional}, nil
}

// validateIdentifierLengths keeps generated constraint and index names within
// the 63-byte PostgreSQL and 64-character MySQL limits.
func validateIdentifierLengths(request resourceRequest) error {
	const limit = 63
	longest := len(request.Plural) + len("_org_id_idx")
	for _, field := range request.Fields {
		longest = max(longest, len(request.Plural)+1+len(field.Name)+len("_length"))
	}
	if longest > limit {
		return fmt.Errorf("the resource and field names are too long for database identifiers (%d > %d characters); shorten them", longest, limit)
	}
	return nil
}

func hasFileNameSuffix(name string) bool {
	segments := strings.Split(name, "_")
	return len(segments) > 1 && slices.Contains(fileNameSuffixes, segments[len(segments)-1])
}

// pluralize applies the built-in rules: consonant+y becomes ies; s, x, z, ch
// and sh take es; everything else takes s. Irregular plurals need --plural.
func pluralize(name string) string {
	switch {
	case strings.HasSuffix(name, "y") && len(name) > 1 && !strings.ContainsRune("aeiou", rune(name[len(name)-2])):
		return name[:len(name)-1] + "ies"
	case strings.HasSuffix(name, "s"), strings.HasSuffix(name, "x"), strings.HasSuffix(name, "z"),
		strings.HasSuffix(name, "ch"), strings.HasSuffix(name, "sh"):
		return name + "es"
	default:
		return name + "s"
	}
}

// initialisms are the words Go spells in capitals (owner_id becomes OwnerID).
var initialisms = map[string]string{
	"api": "API", "html": "HTML", "http": "HTTP", "https": "HTTPS", "id": "ID", "ip": "IP", "json": "JSON",
	"sql": "SQL", "uri": "URI", "url": "URL", "uuid": "UUID", "xml": "XML",
}

// looksPlural reports whether a resource name appears to be a plural already
// and suggests the singular. The generator would otherwise produce
// "productses". Names that end in s, but are singular (status, address, bus,
// analysis), pass.
func looksPlural(name string) (string, bool) {
	switch {
	case strings.HasSuffix(name, "ies") && len(name) > 4:
		return name[:len(name)-3] + "y", true
	case !strings.HasSuffix(name, "s"), strings.HasSuffix(name, "ss"), strings.HasSuffix(name, "us"),
		strings.HasSuffix(name, "is"), strings.HasSuffix(name, "os"):
		return "", false
	case strings.HasSuffix(name, "ses"), strings.HasSuffix(name, "xes"), strings.HasSuffix(name, "zes"),
		strings.HasSuffix(name, "ches"), strings.HasSuffix(name, "shes"):
		return name[:len(name)-2], true
	default:
		return name[:len(name)-1], true
	}
}

// exportedName converts snake_case to an exported Go identifier.
func exportedName(name string) string {
	parts := strings.Split(name, "_")
	for i, part := range parts {
		switch {
		case part == "":
		case initialisms[part] != "":
			parts[i] = initialisms[part]
		default:
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, "")
}

// planResource resolves request against the application in root and renders
// every file. Nothing is written.
func planResource(root string, request resourceRequest, env []string, now time.Time) (resourceSpec, []generatedFile, error) {
	return planResourceWith(root, request, env, now, nil)
}

// planExtension adds files to a plan before it is checked for conflicts, so
// they are covered by the same all-or-nothing, refuse-to-overwrite rules as
// the resource's own files. It may complete the spec.
type planExtension func(projectRoot *os.Root, spec *resourceSpec) ([]generatedFile, error)

func planResourceWith(root string, request resourceRequest, env []string, now time.Time, extend planExtension) (resourceSpec, []generatedFile, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return resourceSpec{}, nil, errors.New("go.mod not found: run this inside a Forge application")
	}
	defer func() { _ = projectRoot.Close() }()
	goMod, err := projectRoot.ReadFile("go.mod")
	if err != nil {
		return resourceSpec{}, nil, errors.New("go.mod not found: run this inside a Forge application")
	}
	module, _, _ := inspectGoMod(goMod)
	if module == "" {
		return resourceSpec{}, nil, errors.New("go.mod has no module declaration")
	}

	adapter, err := databaseAdapter(root, env)
	if err != nil {
		return resourceSpec{}, nil, errors.New("environment files could not be loaded: check .env.development and .env.local syntax")
	}
	switch adapter {
	case "postgres", "mysql", "sqlite":
	case "none":
		return resourceSpec{}, nil, errors.New("this application has no database: generate resources in an application created with a database (FORGE_DATABASE_URL is not set)")
	default:
		return resourceSpec{}, nil, errors.New("FORGE_DATABASE_URL does not select a supported database (postgres, mysql or sqlite)")
	}
	spec := resourceSpec{resourceRequest: request, Module: module, Database: adapter}
	spec.Identity = hasIdentityMigrations(projectRoot, adapter) && !apiOnlyApplication(projectRoot)
	if spec.Identity {
		if spec.Organizations, err = hasOrganizations(projectRoot); err != nil {
			return resourceSpec{}, nil, err
		}
	}
	spec.Organizations = spec.Organizations || request.organizations
	if !spec.Organizations {
		// --global means nothing in an application with one organization:
		// its tables are plain tables, recorded as such.
		spec.Global = false
	}
	switch {
	case request.TenantScoped && !spec.Organizations:
		return resourceSpec{}, nil, errors.New("--org-scoped (--tenant-scoped) is for applications with organizations: this one has a single organization, so its tables are plain tables and need no scope. Create the app with --tenants (forge new NAME --tenants), or convert this one with `forge generate tenancy`")
	case spec.Organizations && !request.TenantScoped && !request.Global:
		return resourceSpec{}, nil, errors.New("choose a scope: --org-scoped (rows belong to one organization: an org_id column every query is filtered by) or --global (rows are shared by every organization)")
	}
	spec.BearerSignIn = spec.Identity && hasBearerSignIn(projectRoot)
	if request.Public && spec.Identity && !request.Model {
		return resourceSpec{}, nil, errors.New("--public is for applications without Forge identity: this one signs users in, so the routes need sign-in and the " + request.Plural + ":read and " + request.Plural + ":write permissions, which the migration grants to the owner role `forge setup` creates")
	}
	if !request.Model {
		spec.seam = openRoutesSeam(projectRoot, module, adapter, spec.Identity)
		spec.Seam = spec.seam.wired
		if request.Public && !spec.Seam {
			return resourceSpec{}, nil, errors.New("--public needs the routes seam (app/bootstrap/routes_gen.go, called from bootstrap.go), which this application predates: paste the printed wiring and write the guard yourself")
		}
	}

	rendered, err := renderResource(spec)
	if err != nil {
		return resourceSpec{}, nil, err
	}
	rerun := false
	migrations, existing, err := planMigrationPair(projectRoot, "create_"+request.Plural, request.Destroy, now, rendered.migrationUp, rendered.migrationDown)
	if err != nil {
		return resourceSpec{}, nil, err
	}
	if existing != "" {
		// The same command run again finds its own migration pair, unchanged
		// (and, checked below, every other file too).
		migrations = identicalMigrationPair(projectRoot, "create_"+request.Plural, now, rendered.migrationUp, rendered.migrationDown)
		if migrations == nil || request.Model {
			return resourceSpec{}, nil, fmt.Errorf("refusing to generate: %s already creates the table %s; pick another resource name or --plural", existing, request.Plural)
		}
		rerun = true
	}
	if spec.Identity && !request.Destroy {
		owner, permission, err := permissionRegisteredBy(projectRoot, "create_"+request.Plural, request.Plural+":read", request.Plural+":write")
		if err != nil {
			return resourceSpec{}, nil, err
		}
		if owner != "" {
			kind := "resource"
			switch {
			case request.Model:
				kind = "model"
			case request.Scaffold:
				kind = "scaffold"
			}
			return resourceSpec{}, nil, fmt.Errorf("refusing to generate: %[1]s already registers the permission %[2]s, which the %[3]s's migration would register too. "+
				"A permission belongs to the one migration that registers it (rolling that migration back removes it and every grant of it), so it never gets a second one. "+
				"Choose another name or --plural; or, to let the %[3]s own %[2]s, revert %[1]s (`forge rollback`) and delete that migration pair first: a controller that asks for %[2]s keeps working with the %[3]s's permission", filepath.ToSlash(owner), permission, kind)
		}
	}
	files := migrations
	for _, source := range rendered.sources {
		path := filepath.Join(filepath.FromSlash(source.dir), request.Name+source.suffix)
		formatted, err := format.Source([]byte(source.content))
		if err != nil {
			return resourceSpec{}, nil, fmt.Errorf("format generated %s: %w", path, err)
		}
		files = append(files, generatedFile{path: path, content: string(formatted)})
	}
	if extend != nil {
		extra, err := extend(projectRoot, &spec)
		if err != nil {
			return resourceSpec{}, nil, err
		}
		files = append(files, extra...)
	}

	if rerun && !allIdentical(projectRoot, files) {
		return resourceSpec{}, nil, fmt.Errorf("refusing to generate: %s already creates the table %s; pick another resource name or --plural", existing, request.Plural)
	}
	check := checkRerunnableFiles
	if request.Model {
		check = checkPlannedFiles
	}
	if err := check(projectRoot, files, request.Destroy); err != nil {
		return resourceSpec{}, nil, err
	}
	return spec, files, nil
}

// planMigrationPair plans the up/down pair named name: at the next free
// version when generating, or, for `forge destroy`, at the version of the pair
// that exists. When generating and a migration of that name already exists,
// existing is its path and nothing is planned.
func planMigrationPair(projectRoot *os.Root, name string, destroy bool, now time.Time, up, down string) (files []generatedFile, existing string, err error) {
	dir := filepath.Join("db", "migrations")
	if destroy {
		if err := rejectExistingSymlinkPaths(projectRoot, []string{dir}); err != nil {
			return nil, "", err
		}
		entries, err := fs.ReadDir(projectRoot.FS(), dir)
		if err != nil {
			return nil, "", errors.New("db/migrations not found: run this inside a Forge application with a database")
		}
		for _, entry := range entries {
			version, rest, ok := strings.Cut(entry.Name(), "_")
			if !ok || len(version) != 14 {
				continue
			}
			switch rest {
			case name + ".up.sql":
				files = append(files, generatedFile{path: filepath.Join(dir, entry.Name()), content: up, migration: true})
			case name + ".down.sql":
				files = append(files, generatedFile{path: filepath.Join(dir, entry.Name()), content: down, migration: true})
			}
		}
		return files, "", nil
	}
	dir, version, err := nextMigrationVersion(projectRoot, now)
	if err != nil {
		return nil, "", err
	}
	entries, err := fs.ReadDir(projectRoot.FS(), dir)
	if err != nil {
		return nil, "", err
	}
	for _, entry := range entries {
		if _, rest, ok := strings.Cut(entry.Name(), "_"); ok && strings.HasPrefix(rest, name+".") {
			return nil, filepath.Join(dir, entry.Name()), nil
		}
	}
	base := filepath.Join(dir, version+"_"+name)
	return []generatedFile{
		{path: base + ".up.sql", content: up, migration: true},
		{path: base + ".down.sql", content: down, migration: true},
	}, "", nil
}

// identicalMigrationPair returns the existing up/down pair named name, marked
// identical, when both exist with exactly the content up and down; else nil.
func identicalMigrationPair(projectRoot *os.Root, name string, now time.Time, up, down string) []generatedFile {
	files, _, err := planMigrationPair(projectRoot, name, true, now, up, down)
	if err != nil || len(files) != 2 {
		return nil
	}
	for i := range files {
		current, err := projectRoot.ReadFile(files[i].path)
		if err != nil || string(current) != files[i].content {
			return nil
		}
		files[i].identical = true
	}
	return files
}

// permissionRegisteredBy finds an up migration in db/migrations, other than
// the pair named own, that names one of permissions as a SQL string literal
// ('posts:read'), and returns its path and the permission it names. A
// permission belongs to the one migration that registers it, because that
// migration's rollback removes it: the generators reuse a registered
// permission (controller) or refuse to register it again (model, resource,
// scaffold) instead of giving it a second owner. Any mention counts, so a
// hand-written migration that registers the permission is found too.
func permissionRegisteredBy(projectRoot *os.Root, own string, permissions ...string) (path, permission string, err error) {
	dir := filepath.Join("db", "migrations")
	entries, err := fs.ReadDir(projectRoot.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	for _, entry := range entries {
		_, rest, ok := strings.Cut(entry.Name(), "_")
		if !ok || entry.IsDir() || !strings.HasSuffix(rest, ".up.sql") || rest == own+".up.sql" {
			continue
		}
		candidate := filepath.Join(dir, entry.Name())
		source, err := projectRoot.ReadFile(candidate)
		if err != nil {
			return "", "", fmt.Errorf("read %s: %w", candidate, err)
		}
		for _, permission := range permissions {
			if strings.Contains(string(source), "'"+permission+"'") {
				return candidate, permission, nil
			}
		}
	}
	return "", "", nil
}

// checkPlannedFiles refuses a plan that would overwrite a file, write through
// a symlink or declare a Go name a package already declares. A destroy plan
// names files that exist, so only the symlink check applies to it.
func checkPlannedFiles(projectRoot *os.Root, files []generatedFile, destroy bool) error {
	return checkPlanned(projectRoot, files, destroy, false)
}

// checkRerunnableFiles is checkPlannedFiles for the generators that wire
// routes (resource, scaffold, controller): a file that already exists with
// exactly the planned content (the same command run again) is marked
// identical and kept, so running one twice with the same arguments changes
// nothing.
func checkRerunnableFiles(projectRoot *os.Root, files []generatedFile, destroy bool) error {
	return checkPlanned(projectRoot, files, destroy, true)
}

func checkPlanned(projectRoot *os.Root, files []generatedFile, destroy, rerunnable bool) error {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.path)
	}
	if err := rejectExistingSymlinkPaths(projectRoot, paths); err != nil {
		return err
	}
	if destroy {
		return nil
	}
	if rerunnable && allIdentical(projectRoot, files) {
		for i := range files {
			files[i].identical = true
		}
		return nil
	}
	for _, path := range paths {
		if _, err := projectRoot.Lstat(path); err == nil {
			return fmt.Errorf("refusing to overwrite %s; nothing was written", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return rejectDeclarationConflicts(projectRoot, files)
}

// allIdentical reports whether every planned file already exists, as a
// regular file, with exactly the planned content: the generator was run
// before with the same arguments.
func allIdentical(projectRoot *os.Root, files []generatedFile) bool {
	for _, file := range files {
		info, err := projectRoot.Lstat(file.path)
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
		current, err := projectRoot.ReadFile(file.path)
		if err != nil || string(current) != file.content {
			return false
		}
	}
	return len(files) > 0
}

// hasIdentityMigrations reports whether the application composes the Forge
// identity migrations, which create the forge_permissions table the generated
// migration seeds: app/bootstrap/app_gen.go imports an auth adapter (forge new
// --auth, forge generate authentication), or, in an application from before
// app_gen.go, cmd/<name>/main.go does. The auth adapter must be the
// database's (adapter, as databaseAdapter reports it).
func hasIdentityMigrations(projectRoot *os.Root, adapter string) bool {
	identity := regexp.MustCompile(`"github.com/rocketinsidecorp/forge-go/auth/` + regexp.QuoteMeta(adapter) + `"`)
	if source, err := projectRoot.ReadFile(scaffold.AppFile); err == nil {
		return identity.Match(source)
	}
	entries, err := fs.ReadDir(projectRoot.FS(), "cmd")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		source, err := fs.ReadFile(projectRoot.FS(), filepath.Join("cmd", entry.Name(), "main.go"))
		if err == nil && identity.Match(source) {
			return true
		}
	}
	return false
}

// rejectDeclarationConflicts refuses to generate Go code that would not
// compile because a package already declares one of the same top-level names,
// for example another resource whose name ends in a generated suffix.
func rejectDeclarationConflicts(projectRoot *os.Root, files []generatedFile) error {
	type key struct{ dir, pkg, name string }
	existing := make(map[key]string)
	directories := make(map[string]bool)
	for _, file := range files {
		if strings.HasSuffix(file.path, ".go") {
			directories[filepath.Dir(file.path)] = true
		}
	}
	fset := token.NewFileSet()
	for dir := range directories {
		entries, err := fs.ReadDir(projectRoot.FS(), dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			source, err := fs.ReadFile(projectRoot.FS(), filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			parsed, err := parser.ParseFile(fset, entry.Name(), source, parser.SkipObjectResolution)
			if err != nil {
				continue
			}
			for _, name := range topLevelNames(parsed) {
				existing[key{dir, parsed.Name.Name, name}] = filepath.Join(dir, entry.Name())
			}
		}
	}
	for _, file := range files {
		if !strings.HasSuffix(file.path, ".go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file.path, file.content, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("generated %s does not parse: %w", file.path, err)
		}
		for _, name := range topLevelNames(parsed) {
			if other, found := existing[key{filepath.Dir(file.path), parsed.Name.Name, name}]; found {
				return fmt.Errorf("%s would declare %s, which %s already declares in package %s; choose another resource name", file.path, name, other, parsed.Name.Name)
			}
		}
	}
	return nil
}

// topLevelNames lists the package-level identifiers a file declares; methods
// do not count because they live in their receiver's namespace.
func topLevelNames(file *ast.File) []string {
	var names []string
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			if declaration.Recv == nil && declaration.Name.Name != "init" {
				names = append(names, declaration.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, spec.Name.Name)
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						if name.Name != "_" {
							names = append(names, name.Name)
						}
					}
				}
			}
		}
	}
	return names
}
