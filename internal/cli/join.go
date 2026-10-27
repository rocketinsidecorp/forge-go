package cli

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/i18n"
	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

const joinUsage = "usage: forge generate join LEFT RIGHT [--dry-run]\n       a LEFT has many RIGHT and a RIGHT has many LEFT, as in: forge generate join post tag"

//go:embed jointemplates/*.tmpl
var joinTemplates embed.FS

// `forge generate join post tag` (decision D23, ADR 0051) is a many-to-many
// association: the table post_tags with a row per post and tag, its foreign
// keys to both sides (composite with org_id in an application with
// organizations, cascading: a join row goes with either side), and the use
// cases and routes that attach, detach and list a post's tags, guarded by
// the two resources' permissions. Both sides are generated resources or
// scaffolds; the first side's pages show the tags in a Turbo Frame.

// joinRequest is a valid `forge generate join` command line.
type joinRequest struct {
	left, right string
	dryRun      bool
	destroy     bool
}

func parseJoinArgs(args []string) (joinRequest, error) {
	var request joinRequest
	var names []string
	for _, arg := range args {
		switch {
		case arg == "--dry-run" && !request.dryRun:
			request.dryRun = true
		case strings.HasPrefix(arg, "-"):
			return joinRequest{}, usageError{fmt.Sprintf("unknown flag %q\n%s", arg, joinUsage)}
		default:
			names = append(names, arg)
		}
	}
	if len(names) != 2 {
		return joinRequest{}, usageError{joinUsage}
	}
	request.left, request.right = names[0], names[1]
	for _, name := range names {
		if err := validateResourceName(name); err != nil {
			return joinRequest{}, err
		}
	}
	if request.left == request.right {
		return joinRequest{}, fmt.Errorf("forge generate join joins two different resources; %s with itself needs a hand-written table", request.left)
	}
	return request, nil
}

// joinView is what the join templates read: the join as a resource named
// LEFT_RIGHT (post_tag, with the table post_tags) whose two references
// fields are the sides.
type joinView struct {
	resourceView
	Left, Right              referenceView
	Camel, PluralCamel       string // postTag, postTags
	LeftPath                 string // /posts
	Attach, Detach           statement
	ListFirst, ListAfter     statement
	Scaffold                 bool // the pages (the first side has pages)
	Organizations, PlatformW bool // writes need the platform guard (global sides with organizations)
}

// joinSpec resolves request against the application: a resource spec for
// LEFT_RIGHT with the two sides as references fields.
func planJoinSpec(projectRoot *os.Root, root string, request joinRequest, env []string) (resourceSpec, error) {
	goMod, err := projectRoot.ReadFile("go.mod")
	if err != nil {
		return resourceSpec{}, errors.New("go.mod not found: run this inside a Forge application")
	}
	module, _, _ := inspectGoMod(goMod)
	adapter, err := databaseAdapter(root, env)
	if err != nil {
		return resourceSpec{}, errors.New("environment files could not be loaded: check .env.development and .env.local syntax")
	}
	switch adapter {
	case "postgres", "mysql", "sqlite":
	case "none":
		return resourceSpec{}, errors.New("this application has no database: a join is a table")
	default:
		return resourceSpec{}, errors.New("FORGE_DATABASE_URL does not select a supported database (postgres, mysql or sqlite)")
	}
	resources, err := recordedResources(projectRoot)
	if err != nil {
		return resourceSpec{}, err
	}
	var sides [2]recordedResource
	for i, name := range []string{request.left, request.right} {
		side, found := findRecorded(resources, name)
		switch {
		case !found:
			return resourceSpec{}, fmt.Errorf("forge generate join %s %s needs the %s resource, and no migration in db/migrations records generating it: generate it first (forge generate scaffold %s ...)", request.left, request.right, name, name)
		case side.request.Model:
			return resourceSpec{}, fmt.Errorf("%s is a model (%s): a join lists and attaches through the JSON routes of both sides, so generate %s as a resource or scaffold", name, side.file, name)
		}
		sides[i] = side
	}
	left, right := sides[0].request, sides[1].request
	if left.Public != right.Public {
		return resourceSpec{}, fmt.Errorf("%s and %s are guarded differently (one was generated with --public): a join serves both sides' rows, so generate it between resources guarded alike", left.Name, right.Name)
	}
	spec := resourceSpec{Module: module, Database: adapter}
	spec.Identity = hasIdentityMigrations(projectRoot, adapter) && !apiOnlyApplication(projectRoot)
	if spec.Identity {
		if spec.Organizations, err = hasOrganizations(projectRoot); err != nil {
			return resourceSpec{}, err
		}
	}
	spec.resourceRequest = resourceRequest{
		Name: left.Name + "_" + right.Name, Plural: joinTable(left, right),
		Fields: []resourceField{
			{Name: left.Name + "_id", Type: "uuid", References: left.Name},
			{Name: right.Name + "_id", Type: "uuid", References: right.Name},
		},
		TenantScoped: spec.Organizations && left.TenantScoped,
		Global:       spec.Organizations && !left.TenantScoped,
		Public:       left.Public,
		Scaffold:     left.Scaffold && !apiOnlyApplication(projectRoot),
		Destroy:      request.destroy,
		DryRun:       request.dryRun,
	}
	if err := validateResourcePlural(spec.Name, spec.Plural, true); err != nil {
		return resourceSpec{}, fmt.Errorf("the join table: %w", err)
	}
	if err := validateIdentifierLengths(spec.resourceRequest); err != nil {
		return resourceSpec{}, err
	}
	for _, resource := range resources {
		if resource.request.Plural == spec.Plural || resource.request.Name == spec.Name {
			return resourceSpec{}, fmt.Errorf("%s already creates %s: the join's table and Go names (%s) would collide with it", resource.file, resource.request.Plural, exportedName(spec.Name))
		}
	}
	if spec.references, err = resolveFields(resources, spec.resourceRequest, spec.Organizations, nil); err != nil {
		return resourceSpec{}, err
	}
	if spec.TenantScoped {
		for i := range spec.references {
			declared, err := declaresUniqueKey(projectRoot, "create_"+spec.Plural, adapter, spec.references[i].parent.Plural)
			if err != nil {
				return resourceSpec{}, err
			}
			spec.references[i].addsKey = !declared
		}
	}
	spec.BearerSignIn = spec.Identity && hasBearerSignIn(projectRoot)
	spec.seam = openRoutesSeam(projectRoot, module, adapter, spec.Identity)
	spec.Seam = spec.seam.wired
	return spec, nil
}

func newJoinView(spec resourceSpec) joinView {
	view := joinView{
		resourceView: newResourceView(spec),
		Camel:        lowerCamel(spec.Name), PluralCamel: lowerCamel(spec.Plural),
		Scaffold:      spec.Scaffold,
		Organizations: spec.Organizations,
		PlatformW:     spec.Identity && spec.Organizations && !spec.TenantScoped,
	}
	view.Left, view.Right = view.References[0], view.References[1]
	view.LeftPath = "/" + view.Left.Plural
	view.Attach, view.Detach, view.ListFirst, view.ListAfter = joinStatements(view)
	return view
}

// joinStatements are the join's SQL: attach (idempotent), detach, and the
// first and later pages of a left row's right rows, newest first.
func joinStatements(v joinView) (attach, detach, first, after statement) {
	q := v.Q
	placeholder := func(n int) string {
		if v.Postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	type builder struct{ args []string }
	add := func(b *builder, arg string) string {
		b.args = append(b.args, arg)
		return placeholder(len(b.args))
	}
	leftColumn, rightColumn := v.Left.Field.Name, v.Right.Field.Name
	{
		var b builder
		var names, values []string
		if v.Tenant {
			names, values = append(names, q("org_id")), append(values, add(&b, "orgID"))
		}
		names = append(names, q(leftColumn), q(rightColumn), q("created_at"))
		values = append(values, add(&b, v.Left.Camel+"ID"), add(&b, v.Right.Camel+"ID"), add(&b, "at"))
		conflict := "\nON CONFLICT DO NOTHING"
		if v.MySQL {
			conflict = "\nON DUPLICATE KEY UPDATE " + q("created_at") + " = " + q("created_at")
		}
		attach = statement{SQL: fmt.Sprintf("INSERT INTO %s (%s)\nVALUES (%s)%s", q(v.Plural), strings.Join(names, ", "), strings.Join(values, ", "), conflict), Args: b.args}
	}
	{
		var b builder
		var predicates []string
		if v.Tenant {
			predicates = append(predicates, q("org_id")+" = "+add(&b, "orgID"))
		}
		predicates = append(predicates, q(leftColumn)+" = "+add(&b, v.Left.Camel+"ID"), q(rightColumn)+" = "+add(&b, v.Right.Camel+"ID"))
		detach = statement{SQL: fmt.Sprintf("DELETE FROM %s WHERE %s", q(v.Plural), strings.Join(predicates, " AND ")), Args: b.args}
	}
	right := v.Right.Parent
	columns := []string{q(right.Plural) + "." + q("id")}
	for _, field := range right.Fields {
		columns = append(columns, q(right.Plural)+"."+q(field.Name))
	}
	for _, column := range []string{"version", "created_at", "updated_at"} {
		columns = append(columns, q(right.Plural)+"."+q(column))
	}
	list := func(paged bool) statement {
		var b builder
		on := q(v.Plural) + "." + q(rightColumn) + " = " + q(right.Plural) + "." + q("id")
		var predicates []string
		if v.Tenant {
			on += " AND " + q(v.Plural) + "." + q("org_id") + " = " + q(right.Plural) + "." + q("org_id")
			predicates = append(predicates, q(v.Plural)+"."+q("org_id")+" = "+add(&b, "orgID"))
		}
		predicates = append(predicates, q(v.Plural)+"."+q(leftColumn)+" = "+add(&b, v.Left.Camel+"ID"))
		if paged {
			predicates = append(predicates, q(right.Plural)+"."+q("id")+" < "+add(&b, "after"))
		}
		return statement{
			SQL: fmt.Sprintf("SELECT %s\nFROM %s JOIN %s ON %s\nWHERE %s\nORDER BY %s DESC LIMIT %s",
				strings.Join(columns, ", "), q(right.Plural), q(v.Plural), on, strings.Join(predicates, " AND "), q(right.Plural)+"."+q("id"), add(&b, "limit")),
			Args: b.args,
		}
	}
	return attach, detach, list(false), list(true)
}

// joinMigration creates the join table.
func joinMigration(v joinView) (up, down string) {
	q := v.Q
	uuidType, timeType := "uuid", "timestamptz"
	switch {
	case v.MySQL:
		uuidType, timeType = "CHAR(36)", "DATETIME(6)"
	case v.SQLite:
		uuidType, timeType = "TEXT", "TEXT"
	}
	var columns []column
	if v.Tenant {
		switch {
		case v.MySQL:
			columns = append(columns, column{q("org_id"), mysqlOrgIDType + " NOT NULL"})
		default:
			columns = append(columns, column{q("org_id"), uuidType + " NOT NULL REFERENCES forge_tenants (id)"})
		}
	}
	columns = append(columns,
		column{q(v.Left.Field.Name), uuidType + " NOT NULL"},
		column{q(v.Right.Field.Name), uuidType + " NOT NULL"},
		column{q("created_at"), timeType + " NOT NULL"})
	width := 0
	for _, c := range columns {
		width = max(width, len(c.name))
	}
	var lines []string
	for _, c := range columns {
		lines = append(lines, fmt.Sprintf("    %-*s %s", width, c.name, c.definition))
	}
	lines = append(lines, fmt.Sprintf("    PRIMARY KEY (%s, %s)", referenceColumns(v.resourceView, v.Left.Field.Name), q(v.Right.Field.Name)))
	rightIndex := q(v.Plural + "_" + v.Right.Field.Name + "_idx")
	if v.MySQL {
		lines = append(lines, fmt.Sprintf("    INDEX %s (%s)", rightIndex, referenceColumns(v.resourceView, v.Right.Field.Name)))
		if v.Tenant {
			lines = append(lines, fmt.Sprintf("    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES forge_tenants (id)", q(v.Plural+"_org_id_fk"), q("org_id")))
		}
	}
	for _, side := range []referenceView{v.Left, v.Right} {
		lines = append(lines, fmt.Sprintf("    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE CASCADE",
			q(side.Constraint), referenceColumns(v.resourceView, side.Field.Name), q(side.Plural), referenceColumns(v.resourceView, "id")))
	}
	var sql strings.Builder
	fmt.Fprintf(&sql, "-- forge:generate join %s %s\n", v.Left.Name, v.Right.Name)
	fmt.Fprintf(&sql, "-- Joins %[1]s and %[2]s: a %[3]s has many %[2]s and a %[4]s has many %[1]s. A row goes\n-- with either side (ON DELETE CASCADE).\n", v.Left.Plural, v.Right.Plural, v.Left.Human, v.Right.Human)
	for _, side := range []referenceView{v.Left, v.Right} {
		if side.AddsKey {
			fmt.Fprintf(&sql, "-- The foreign key (org_id, %s) needs (org_id, id) declared unique on %s.\n", side.Field.Name, side.Plural)
			fmt.Fprintf(&sql, "CREATE UNIQUE INDEX %s ON %s (%s, %s);\n", q(uniqueKeyName(side.Plural)), q(side.Plural), q("org_id"), q("id"))
		}
	}
	fmt.Fprintf(&sql, "CREATE TABLE %s (\n%s\n)", q(v.Plural), strings.Join(lines, ",\n"))
	switch {
	case v.MySQL:
		sql.WriteString(" ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	case v.SQLite:
		sql.WriteString(" STRICT")
	}
	sql.WriteString(";\n")
	if !v.MySQL {
		fmt.Fprintf(&sql, "\n-- The %s's %s: the primary key serves the other side.\nCREATE INDEX %s ON %s (%s);\n", v.Right.Human, v.Left.Plural, rightIndex, q(v.Plural), referenceColumns(v.resourceView, v.Right.Field.Name))
	}
	if v.Tenant && v.Postgres {
		sql.WriteString(rowSecurity(v.Plural, q))
	}
	var rollback strings.Builder
	fmt.Fprintf(&rollback, "DROP TABLE %s;\n", q(v.Plural))
	for _, side := range []referenceView{v.Left, v.Right} {
		if side.AddsKey {
			if v.MySQL {
				fmt.Fprintf(&rollback, "DROP INDEX %s ON %s;\n", q(uniqueKeyName(side.Plural)), q(side.Plural))
			} else {
				fmt.Fprintf(&rollback, "DROP INDEX %s;\n", q(uniqueKeyName(side.Plural)))
			}
		}
	}
	return sql.String(), rollback.String()
}

// planJoin renders every file of the join and plans the seam and the first
// side's associations partial. Nothing is written.
func planJoin(root string, request joinRequest, env []string, now time.Time) (resourceSpec, []generatedFile, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return resourceSpec{}, nil, errors.New("go.mod not found: run this inside a Forge application")
	}
	defer func() { _ = projectRoot.Close() }()
	spec, err := planJoinSpec(projectRoot, root, request, env)
	if err != nil {
		return resourceSpec{}, nil, err
	}
	view := newJoinView(spec)
	up, down := joinMigration(view)
	files, existing, err := planMigrationPair(projectRoot, "create_"+spec.Plural, request.destroy, now, up, down)
	if err != nil {
		return resourceSpec{}, nil, err
	}
	rerun := false
	if existing != "" {
		if files = identicalMigrationPair(projectRoot, "create_"+spec.Plural, now, up, down); files == nil {
			return resourceSpec{}, nil, fmt.Errorf("refusing to generate: %s already creates the table %s", existing, spec.Plural)
		}
		rerun = true
	}
	sources := []struct{ path, template string }{
		{filepath.Join("app", "domain", spec.Plural+".go"), "domain.go.tmpl"},
		{filepath.Join("app", "application", spec.Plural+".go"), "application.go.tmpl"},
		{filepath.Join("app", "application", spec.Plural+"_test.go"), "application_test.go.tmpl"},
		{filepath.Join("app", "adapters", "persistence", spec.Plural+".go"), map[bool]string{true: "persistence_postgres.go.tmpl", false: "persistence_sql.go.tmpl"}[view.Postgres]},
		{filepath.Join("app", "adapters", "persistence", spec.Plural+"_test.go"), "persistence_test.go.tmpl"},
		{filepath.Join("app", "adapters", "httpapi", spec.Plural+".go"), "httpapi.go.tmpl"},
		{filepath.Join("app", "adapters", "httpapi", spec.Plural+"_test.go"), "httpapi_test.go.tmpl"},
	}
	if spec.Scaffold {
		sources = append(sources,
			struct{ path, template string }{filepath.Join("app", "adapters", "htmlui", spec.Plural+".go"), "htmlui.go.tmpl"},
			struct{ path, template string }{filepath.Join("app", "adapters", "htmlui", spec.Plural+"_test.go"), "htmlui_test.go.tmpl"},
		)
	}
	for _, source := range sources {
		content, err := renderEmbedded(joinTemplates, "jointemplates/"+source.template, view)
		if err != nil {
			return resourceSpec{}, nil, err
		}
		formatted, err := format.Source([]byte(content))
		if err != nil {
			return resourceSpec{}, nil, fmt.Errorf("format generated %s: %w", source.path, err)
		}
		files = append(files, generatedFile{path: source.path, content: string(formatted)})
	}
	if spec.Scaffold {
		page, err := renderEmbedded(joinTemplates, "jointemplates/index.gohtml.tmpl", view)
		if err != nil {
			return resourceSpec{}, nil, err
		}
		files = append(files, generatedFile{path: filepath.Join("app", "views", spec.Plural, "index.gohtml"), content: page})
		locales := []string{i18n.DefaultLocale}
		if locale := appDefaultLocale(projectRoot); locale != i18n.DefaultLocale {
			locales = append(locales, locale)
		}
		for _, locale := range locales {
			translations, err := joinTranslations(view, locale)
			if err != nil {
				return resourceSpec{}, nil, err
			}
			files = append(files, generatedFile{path: filepath.Join(localesDir, spec.Plural+"."+locale+".json"), content: translations, translation: locale != i18n.DefaultLocale})
		}
		if spec.rewrites, err = joinRewrites(projectRoot, spec); err != nil {
			return resourceSpec{}, nil, err
		}
	}
	if rerun && !allIdentical(projectRoot, files) {
		return resourceSpec{}, nil, fmt.Errorf("refusing to generate: %s already creates the table %s", existing, spec.Plural)
	}
	if err := checkRerunnableFiles(projectRoot, files, request.destroy); err != nil {
		return resourceSpec{}, nil, err
	}
	return spec, files, nil
}

// joinRewrites add the join's frame to the first side's associations partial
// (or, for destroy, take it out).
func joinRewrites(projectRoot *os.Root, spec resourceSpec) ([]fileRewrite, error) {
	left := spec.references[0].parent
	right := spec.references[1].parent
	natural, err := associationEntries(projectRoot, left)
	if err != nil {
		return nil, err
	}
	own := joinEntry(left, right, "~")
	var without []associationEntry
	present := false
	for _, entry := range natural {
		if entry.frame == own.frame {
			present = true
			continue
		}
		without = append(without, entry)
	}
	path := associationsPath(left.Plural)
	switch {
	case spec.Destroy && present:
		return []fileRewrite{{path: path, from: renderAssociations(left, natural), to: renderAssociations(left, without)}}, nil
	case !spec.Destroy && !present:
		return []fileRewrite{{path: path, from: renderAssociations(left, natural), to: renderAssociations(left, append(natural, own))}}, nil
	}
	return nil, nil
}

// joinTranslations is config/locales/post_tags.LOCALE.json: the English text
// of the page, under locale.
func joinTranslations(v joinView, locale string) (string, error) {
	rights := strings.ReplaceAll(v.Right.Plural, "_", " ")
	scope := map[string]any{
		"back": "Back to the " + v.Left.Human,
		"form": map[string]any{
			"choose": "Choose a " + v.Right.Human,
			"label":  "Add a " + v.Right.Human,
			"submit": "Add " + v.Right.Human,
		},
		"index": map[string]any{
			"empty":     "No " + rights + " yet.",
			"next_page": "Next page",
			"remove":    "Remove",
			"title":     capitalize(rights),
		},
	}
	content, err := json.MarshalIndent(map[string]any{locale: map[string]any{v.Plural: scope}}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(content) + "\n", nil
}

// joinWiring is the join's block of the routes seam (or the printed wiring):
// its use cases on the two sides' repositories, the JSON routes and, with
// pages, the HTML ones, guarded like the resources' routes.
func joinWiring(spec resourceSpec, view joinView) resourceWiringParts {
	parts := buildResourceWiring(spec)
	service := view.Camel + "Service"
	guard := func(name string) string {
		reference := spec.guard(name)
		if !view.PlatformW {
			return reference
		}
		// Global sides: attaching and detaching write rows every
		// organization reads, so they stay behind the platform guard.
		return fmt.Sprintf(`func(permission string, next http.Handler) (http.Handler, error) {
		if permission == %q {
			return protectPlatformWrite(permission, next)
		}
		return %s(permission, next)
	}`, view.Left.Plural+":write", reference)
	}
	usage := fmt.Sprintf(`	%[1]s, err := application.New%[2]sService(persistence.New%[2]sRepository(database), persistence.New%[3]sRepository(database), persistence.New%[4]sRepository(database), time.Now)
	if err != nil {
		return err
	}
	if err := httpapi.Register%[5]s(app, %[1]s, %[6]s); err != nil {
		return err
	}
`, service, view.Title, view.Left.Title, view.Right.Title, view.PluralTitle, guard("protect"))
	if spec.Scaffold {
		cookie := "protect"
		if spec.Identity {
			cookie = "protectCookie"
		}
		usage += fmt.Sprintf(`	if err := htmlui.Register%[1]s(app, %[2]s, renderer, %[3]s); err != nil {
		return err
	}
`, view.PluralTitle, service, guard(cookie))
	}
	parts.Usage = usage
	parts.Notes = []string{fmt.Sprintf("A %[1]s's %[2]s are listed at GET /v1/%[3]s/{%[4]s}/%[2]s (%[3]s:read and %[2]s:read), attached with POST and detached with DELETE /v1/%[3]s/{%[4]s}/%[2]s/{%[5]s} (%[3]s:write and %[2]s:read).", view.Left.Human, view.Right.Plural, view.Left.Plural, view.Left.Field.Name, view.Right.Field.Name)}
	if spec.Scaffold {
		parts.Notes = append(parts.Notes, fmt.Sprintf("The %[1]s page shows its %[2]s, with forms to add and remove them, in a Turbo Frame (/%[3]s/{id}/%[2]s).", view.Left.Human, view.Right.Plural, view.Left.Plural))
	}
	return parts
}

// planJoinSeam plans the seam with the join's block added or removed.
func planJoinSeam(root string, spec resourceSpec, view joinView, remove bool) (seamWiring, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return seamWiring{}, err
	}
	defer func() { _ = projectRoot.Close() }()
	parts := joinWiring(spec, view)
	content, verb, err := planRoutes(projectRoot, spec.seam, routesChange{
		entry:  scaffold.RouteBlock{Kind: "join", Name: spec.Plural, Body: parts.Usage},
		shared: sharedFromParts(parts),
		remove: remove,
	})
	return seamWiring{content: content, verb: verb, notes: parts.Notes}, err
}

// runGenerateJoin is `forge generate join LEFT RIGHT`.
func runGenerateJoin(args []string, stdout io.Writer, root string, env []string, now time.Time) error {
	request, err := parseJoinArgs(args)
	if err != nil {
		return err
	}
	spec, files, err := planJoin(root, request, env, now)
	if err != nil {
		return err
	}
	view := newJoinView(spec)
	var wiring seamWiring
	if spec.Seam {
		if wiring, err = planJoinSeam(root, spec, view, false); err != nil {
			return err
		}
	}
	rewrites, err := checkRewritesAt(root, spec.rewrites, true)
	if err != nil {
		return err
	}
	if !request.dryRun {
		if err := applyGeneratedFiles(root, directoriesOf(newFiles(files)), newFiles(files)); err != nil {
			return err
		}
		if spec.Seam && wiring.verb == "wire" {
			if err := writeRoutes(root, wiring.content); err != nil {
				return err
			}
		}
		if err := applyRewrites(root, rewrites); err != nil {
			return err
		}
	}
	if err := reportRewrites(stdout, rewrites, request.dryRun); err != nil {
		return err
	}
	verb := map[bool]string{false: "create", true: "would create"}[request.dryRun]
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
		_, err := io.WriteString(stdout, formatWiring("join", "join", joinWiring(spec, view), "forge migrate && forge test"))
		return err
	}
	_, err = io.WriteString(stdout, reportSeam(wiring, request.dryRun, "forge migrate && forge test"))
	return err
}

// planDestroyJoin is `forge destroy join LEFT RIGHT`.
func planDestroyJoin(args []string, root string, env []string, now time.Time) (destroyPlan, error) {
	request, err := parseJoinArgs(args)
	if err != nil {
		return destroyPlan{}, err
	}
	request.destroy = true
	spec, files, err := planJoin(root, request, env, now)
	if err != nil {
		return destroyPlan{}, err
	}
	view := newJoinView(spec)
	plan := destroyPlan{files: files, dryRun: request.dryRun, unwire: "the join wiring:\n" + joinWiring(spec, view).Usage, rewrites: spec.rewrites}
	if spec.Seam {
		wiring, err := planJoinSeam(root, spec, view, true)
		if err != nil {
			return destroyPlan{}, err
		}
		plan.routes, plan.routesVerb, plan.unwire = wiring.content, wiring.verb, ""
	}
	return plan, nil
}
