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
)

const resourceUsage = "usage: forge generate resource NAME field:type... (--tenant-scoped | --global) [--plural PLURAL] [--dry-run]"

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

// reservedColumns are the columns every generated table owns.
var reservedColumns = []string{"id", "tenant_id", "version", "created_at", "updated_at"}

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
	DryRun       bool
	// Scaffold is set by `forge generate scaffold`: the resource plus its
	// server-rendered HTML pages. It changes only what is generated around
	// the resource, never the resource itself.
	Scaffold bool
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
	// BrowserPipeline reports that app/bootstrap/bootstrap.go appears to
	// install the CSRF, method-override and flash middleware server-rendered
	// forms rely on. Only scaffolds look.
	BrowserPipeline bool
	// ViewRenderer reports that bootstrap.go already builds the view
	// renderer (renderer, err := views.New()), as PostgreSQL applications
	// from `forge new` do for the sign-in pages. Only scaffolds look.
	ViewRenderer bool
}

func runGenerateResource(args []string, stdout io.Writer, root string, env []string, now time.Time) error {
	request, err := parseResourceArgs(args)
	if err != nil {
		return err
	}
	spec, files, err := planResource(root, request, env, now)
	if err != nil {
		return err
	}
	if !request.DryRun {
		directories := []string{"db/migrations", "app/domain", "app/application", "app/adapters/persistence", "app/adapters/httpapi"}
		if err := applyGeneratedFiles(root, directories, files); err != nil {
			return err
		}
	}
	return reportGenerated(stdout, spec, files)
}

// reportGenerated lists the files (planned, for --dry-run) and prints the
// wiring the application owner adds to bootstrap.go.
func reportGenerated(stdout io.Writer, spec resourceSpec, files []generatedFile) error {
	verb := "create"
	if spec.DryRun {
		verb = "would create"
	}
	for _, file := range files {
		if _, err := fmt.Fprintf(stdout, "  %s  %s\n", verb, file.path); err != nil {
			return err
		}
	}
	_, err := io.WriteString(stdout, resourceWiring(spec))
	return err
}

// parseResourceArgs reads `NAME field:type... --tenant-scoped|--global
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
		case arg == "--tenant-scoped":
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
			return resourceRequest{}, fmt.Errorf("unknown flag %q\n%s", arg, usage)
		default:
			positional = append(positional, arg)
		}
	}
	switch {
	case tenantScoped && global:
		return resourceRequest{}, errors.New("--tenant-scoped and --global are mutually exclusive: choose exactly one")
	case !tenantScoped && !global:
		return resourceRequest{}, errors.New("choose a scope: --tenant-scoped (rows belong to one tenant) or --global (rows are shared by all tenants)")
	}
	request.TenantScoped = tenantScoped
	if len(positional) == 0 {
		return resourceRequest{}, errors.New(usage)
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
	name, kind, ok := strings.Cut(declaration, ":")
	if !ok {
		return resourceField{}, fmt.Errorf("field %q must be written name:type (types: %s)", declaration, strings.Join(resourceFieldTypes, ", "))
	}
	optional := false
	if trimmed, found := strings.CutSuffix(kind, "?"); found {
		kind, optional = trimmed, true
	}
	switch {
	case len(name) > maxResourceNameLength || !resourceName.MatchString(name):
		return resourceField{}, fmt.Errorf("field name %q must be snake_case (lowercase letters and digits, starting with a letter, at most %d characters)", name, maxResourceNameLength)
	case slices.Contains(reservedColumns, name):
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
	longest := len(request.Plural) + len("_tenant_id_idx")
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
	spec.Identity = adapter == "postgres" && hasIdentityMigrations(projectRoot)

	dir, version, err := nextMigrationVersion(projectRoot, now)
	if err != nil {
		return resourceSpec{}, nil, err
	}
	existingMigrations, err := fs.ReadDir(projectRoot.FS(), dir)
	if err != nil {
		return resourceSpec{}, nil, err
	}
	for _, entry := range existingMigrations {
		if _, rest, ok := strings.Cut(entry.Name(), "_"); ok && strings.HasPrefix(rest, "create_"+request.Plural+".") {
			return resourceSpec{}, nil, fmt.Errorf("refusing to generate: %s already creates the table %s; pick another resource name or --plural", filepath.Join(dir, entry.Name()), request.Plural)
		}
	}
	migrationBase := filepath.Join(dir, version+"_create_"+request.Plural)
	rendered, err := renderResource(spec)
	if err != nil {
		return resourceSpec{}, nil, err
	}
	files := []generatedFile{
		{path: migrationBase + ".up.sql", content: rendered.migrationUp},
		{path: migrationBase + ".down.sql", content: rendered.migrationDown},
	}
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

	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.path)
	}
	if err := rejectExistingSymlinkPaths(projectRoot, paths); err != nil {
		return resourceSpec{}, nil, err
	}
	for _, path := range paths {
		if _, err := projectRoot.Lstat(path); err == nil {
			return resourceSpec{}, nil, fmt.Errorf("refusing to overwrite %s; nothing was written", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return resourceSpec{}, nil, err
		}
	}
	if err := rejectDeclarationConflicts(projectRoot, files); err != nil {
		return resourceSpec{}, nil, err
	}
	return spec, files, nil
}

// hasIdentityMigrations reports whether the application composes the Forge
// identity migrations, which create the forge_permissions table the generated
// migration seeds. PostgreSQL applications from `forge new` always do.
func hasIdentityMigrations(projectRoot *os.Root) bool {
	entries, err := fs.ReadDir(projectRoot.FS(), "cmd")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		source, err := fs.ReadFile(projectRoot.FS(), filepath.Join("cmd", entry.Name(), "main.go"))
		if err == nil && strings.Contains(string(source), `"github.com/rocketinsidecorp/forge-go/auth/postgres"`) {
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
