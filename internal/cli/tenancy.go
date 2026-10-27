package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

const tenancyUsage = "usage: forge generate tenancy [--org SLUG] [--dry-run]"

// runGenerateTenancy is `forge generate tenancy` (decision D36): it converts
// a single-organization application with Forge identity (forge new --auth)
// into one with organizations (forge new --tenants). Every table a generator
// created gets an org_id column, filled with the application's one
// organization, indexed and required; on PostgreSQL it gets the row-level
// security policy new org-scoped tables get. The generated Go code of those
// tables is rendered again org-scoped, and app_gen.go flips the application,
// so sign-in asks for the organization and generators need a scope. The
// migration's down reverts the schema (refusing while rows of other
// organizations exist); the code returns with version control.
func runGenerateTenancy(args []string, stdout io.Writer, root string, env []string, now time.Time) error {
	dryRun, slug := false, ""
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--dry-run" && !dryRun:
			dryRun = true
		case arg == "--org" && slug == "" && i+1 < len(args):
			i++
			slug = args[i]
		case strings.HasPrefix(arg, "--org=") && slug == "":
			slug = strings.TrimPrefix(arg, "--org=")
		default:
			return usageError{tenancyUsage}
		}
	}
	if slug != "" {
		if _, canonical, ok := auth.NormalizeIdentity("owner@example.com", slug); !ok || canonical != slug || slug == auth.DefaultTenantSlug {
			return fmt.Errorf("--org %q must be a new organization slug: a lowercase letter, then 1 to 62 lowercase letters, digits or hyphens", slug)
		}
	}
	plan, err := planTenancy(root, env, now, slug)
	if err != nil {
		return err
	}
	if !dryRun {
		if err := applyTenancy(root, plan); err != nil {
			return err
		}
	}
	verb := map[bool]string{false: "", true: "would "}[dryRun]
	var out strings.Builder
	for _, file := range plan.migrations {
		fmt.Fprintf(&out, "  %screate  %s\n", verb, file.path)
	}
	for _, file := range plan.converted {
		fmt.Fprintf(&out, "  %sconvert  %s\n", verb, file.path)
	}
	fmt.Fprintf(&out, "  %srewrite  %s\n", verb, scaffold.AppFile)
	if plan.agents.content != nil {
		fmt.Fprintf(&out, "  %srewrite  %s\n", verb, scaffold.AgentsFile)
	}
	if plan.agents.note != "" {
		fmt.Fprintf(&out, "\n  %s\n", plan.agents.note)
	}
	if len(plan.tables) > 0 {
		fmt.Fprintf(&out, "\n  Org-scoped now: %s (an org_id column every query is filtered by%s).\n", strings.Join(plan.tables, ", "), map[bool]string{true: ", behind row-level security", false: ""}[plan.adapter == "postgres"])
	}
	for _, table := range plan.kept {
		fmt.Fprintf(&out, "\n  %s stays global: it was generated with --public, which an org-scoped table cannot be (an anonymous request names no organization). Destroy it and generate it again with --org-scoped to scope it.\n", table)
	}
	for _, table := range plan.unrecorded {
		fmt.Fprintf(&out, "\n  %s stays as it is: %s creates it without a generator header (hand-written, or in a forge generate migration file), so tenancy cannot tell how your code reads it. If its rows belong to an organization, add org_id to it and its queries by hand.\n", table.table, table.file)
	}
	organization := auth.DefaultTenantSlug
	if slug != "" {
		organization = slug
	}
	fmt.Fprintf(&out, "\n  The application now has organizations: sign-in asks for one, setup takes --org, and generators need --org-scoped or --global. Existing users and rows belong to the organization %q.\n", organization)
	out.WriteString("\n  The migration checks first and changes nothing when it refuses: forge migrate refuses while more than one organization exists, or while the tables hold rows and forge setup never ran; forge rollback refuses while more than one organization exists, or a row names another one.\n")
	out.WriteString("\n  Then: forge migrate && forge test\n")
	_, err = io.WriteString(stdout, out.String())
	return err
}

type tenancyPlan struct {
	adapter    string
	migrations []generatedFile
	converted  []generatedFile
	app        []byte
	tables     []string
	kept       []string
	unrecorded []unrecordedTable
	// agents is AGENTS.md, refreshed for an application with organizations.
	agents agentsRefresh
}

// recordedCommand finds the generator command a create migration records.
var recordedCommand = regexp.MustCompile(`(?m)^-- forge:generate ((?:model|resource|scaffold) .+)$`)

func planTenancy(root string, env []string, now time.Time, slug string) (tenancyPlan, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return tenancyPlan{}, errors.New("go.mod not found: run this inside a Forge application")
	}
	defer func() { _ = projectRoot.Close() }()
	goMod, err := projectRoot.ReadFile("go.mod")
	if err != nil {
		return tenancyPlan{}, errors.New("go.mod not found: run this inside a Forge application")
	}
	module, _, _ := inspectGoMod(goMod)
	adapter, err := databaseAdapter(root, env)
	if err != nil {
		return tenancyPlan{}, errors.New("environment files could not be loaded: check .env.development and .env.local syntax")
	}
	features, present, err := applicationFeatures(projectRoot)
	switch {
	case err != nil:
		return tenancyPlan{}, err
	case !present || !features.Identity:
		return tenancyPlan{}, errors.New("forge generate tenancy converts an application with Forge identity (forge new --auth): organizations are who signs in, and this application signs nobody in. Run `forge generate authentication` first")
	case features.Organizations:
		return tenancyPlan{}, errors.New("this application has organizations already (app/bootstrap/app_gen.go: Organizations = true); nothing was written")
	}
	switch adapter {
	case "postgres", "mysql", "sqlite":
	default:
		return tenancyPlan{}, errors.New("FORGE_DATABASE_URL does not select a supported database (postgres, mysql or sqlite)")
	}
	plan := tenancyPlan{adapter: adapter}
	features.Module, features.Adapter, features.Organizations = module, adapter, true
	if plan.app, err = scaffold.RenderApp(features); err != nil {
		return tenancyPlan{}, err
	}

	recorded, unrecorded, err := recordedCommands(projectRoot)
	if err != nil {
		return tenancyPlan{}, err
	}
	plan.unrecorded = unrecorded
	if err := refuseAssociations(projectRoot, recorded); err != nil {
		return tenancyPlan{}, err
	}
	for _, migration := range recorded {
		command := migration.command
		request, extend, err := parseRecordedCommand(command)
		if err != nil {
			return tenancyPlan{}, err
		}
		if request.Public {
			plan.kept = append(plan.kept, request.Plural)
			continue
		}
		if request.TenantScoped {
			// No generator writes --org-scoped into an application without
			// organizations, so the header was edited: converting would
			// trust it blindly, and skipping it would leave a table global
			// unannounced.
			return tenancyPlan{}, fmt.Errorf("refusing to convert: %s records `forge generate %s`, which an application without organizations cannot have generated (its header was edited), so the scope of %s is unknown; nothing was written. Restore the header the generator wrote, or remove the line if %s is hand-written", migration.file, command, request.Plural, request.Plural)
		}
		before, err := renderedSources(root, request, env, now, extend)
		if err != nil {
			return tenancyPlan{}, err
		}
		for _, file := range before {
			current, err := projectRoot.ReadFile(file.path)
			if err != nil || string(current) != file.content {
				return tenancyPlan{}, fmt.Errorf("refusing to convert %s: %s is not what `forge generate %s` wrote (it was changed, or written by another Forge version), so rendering it org-scoped would lose those changes; nothing was written. Make its org_id scoping by hand, or restore the file, and run the command again", request.Plural, file.path, command)
			}
		}
		request.TenantScoped, request.organizations = true, true
		after, err := renderedSources(root, request, env, now, extend)
		if err != nil {
			return tenancyPlan{}, err
		}
		for _, file := range after {
			if current, err := projectRoot.ReadFile(file.path); err == nil && string(current) == file.content {
				continue
			}
			plan.converted = append(plan.converted, file)
		}
		plan.tables = append(plan.tables, request.Plural)
	}
	up, down := tenancyMigration(adapter, plan.tables, slug)
	migrations, existing, err := planMigrationPair(projectRoot, "add_organizations", false, now, up, down)
	if err != nil {
		return tenancyPlan{}, err
	}
	if existing != "" {
		return tenancyPlan{}, fmt.Errorf("refusing to generate: %s exists already; nothing was written", existing)
	}
	plan.migrations = migrations
	name := "app"
	if command, err := resolveCommand(root, nil); err == nil {
		if _, err := scaffold.Normalize(scaffold.Options{Name: command}); err == nil {
			name = command
		}
	}
	plan.agents, err = planAgentsRefresh(projectRoot, scaffold.Options{Name: name, Module: module, Database: newDatabaseOption(projectRoot, adapter), Auth: true},
		func(*scaffold.Options) {}, func(options *scaffold.Options) { options.Tenants = true })
	if err != nil {
		return tenancyPlan{}, err
	}
	paths := []string{scaffold.AppFile}
	if plan.agents.content != nil {
		paths = append(paths, scaffold.AgentsFile)
	}
	for _, file := range append(slices.Clone(plan.migrations), plan.converted...) {
		paths = append(paths, file.path)
	}
	if err := rejectExistingSymlinkPaths(projectRoot, paths); err != nil {
		return tenancyPlan{}, err
	}
	return plan, nil
}

// refuseAssociations refuses to convert an application whose tables
// reference each other (ADR 0051). With organizations every foreign key
// between org-scoped tables must be composite, (org_id, post_id) -> posts
// (org_id, id), so that no row can reference another organization's; the
// conversion would have to replace each plain foreign key, and SQLite can
// change a foreign key only by rebuilding the table. Rather than half-convert
// them, tenancy refuses and says what to do.
func refuseAssociations(projectRoot *os.Root, recorded []recordedMigration) error {
	var found []string
	for _, migration := range recorded {
		request, _, err := parseRecordedCommand(migration.command)
		if err != nil {
			return err
		}
		for _, field := range request.Fields {
			if field.References != "" {
				found = append(found, fmt.Sprintf("%s:references in %s", field.References, migration.file))
			}
		}
	}
	joins, err := recordedJoins(projectRoot)
	if err != nil {
		return err
	}
	for _, join := range joins {
		found = append(found, fmt.Sprintf("the join %s %s in %s", join.left, join.right, join.file))
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to convert: the application has associations (%s), and with organizations their foreign keys must become composite, (org_id, post_id) -> posts (org_id, id), which forge generate tenancy does not rewrite (SQLite cannot alter a foreign key without rebuilding the table). Nothing was written. Convert before generating associations, or create the application with --tenants", strings.Join(found, "; "))
}

// recordedMigration is a create migration's file and the generator command
// it records.
type recordedMigration struct {
	file    string
	command string
}

// unrecordedTable is a table a migration without a generator header creates.
type unrecordedTable struct {
	table string
	file  string
}

// createTable finds the tables a migration creates.
var createTable = regexp.MustCompile("(?im)^\\s*CREATE\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?[`\"]?([A-Za-z_][A-Za-z0-9_]*)")

// recordedCommands are the generator commands the create migrations of
// db/migrations record, in migration order, and the tables migrations
// without a generator header create (hand-written ones, or written into a
// `forge generate migration` file): tenancy cannot know how their code reads
// them, so it leaves them as they are and says so.
func recordedCommands(projectRoot *os.Root) ([]recordedMigration, []unrecordedTable, error) {
	dir := filepath.Join("db", "migrations")
	entries, err := fs.ReadDir(projectRoot.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var (
		commands   []recordedMigration
		unrecorded []unrecordedTable
	)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		file := filepath.ToSlash(filepath.Join(dir, entry.Name()))
		source, err := projectRoot.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, nil, err
		}
		if match := recordedCommand.FindSubmatch(source); match != nil {
			commands = append(commands, recordedMigration{file: file, command: string(match[1])})
			continue
		}
		for _, match := range createTable.FindAllSubmatch(source, -1) {
			unrecorded = append(unrecorded, unrecordedTable{table: string(match[1]), file: file})
		}
	}
	return commands, unrecorded, nil
}

// parseRecordedCommand is the request a recorded command line makes.
func parseRecordedCommand(command string) (resourceRequest, planExtension, error) {
	fields := strings.Fields(command)
	var (
		request resourceRequest
		err     error
		extend  planExtension
	)
	switch fields[0] {
	case "model":
		request, err = parseModelArgs(fields[1:])
	case "scaffold":
		request, err = parseGeneratorArgs(fields[1:], scaffoldUsage)
		request.Scaffold, extend = true, planScaffoldFiles
	default:
		request, err = parseGeneratorArgs(fields[1:], resourceUsage)
	}
	if err != nil {
		return resourceRequest{}, nil, fmt.Errorf("the migration records `forge generate %s`, which no longer parses: %w", command, err)
	}
	return request, extend, nil
}

// renderedSources renders request's own Go sources (not its migration, nor
// files other generated code shares), as `forge destroy` finds them.
func renderedSources(root string, request resourceRequest, env []string, now time.Time, extend planExtension) ([]generatedFile, error) {
	request.Destroy = true
	_, files, err := planResourceWith(root, request, env, now, extend)
	if err != nil {
		return nil, err
	}
	var sources []generatedFile
	for _, file := range files {
		if !file.migration && !file.shared && strings.HasSuffix(file.path, ".go") {
			sources = append(sources, file)
		}
	}
	return sources, nil
}

// applyTenancy writes the migration, the converted sources and app_gen.go.
func applyTenancy(root string, plan tenancyPlan) error {
	if err := applyGeneratedFiles(root, directoriesOf(plan.migrations), plan.migrations); err != nil {
		return err
	}
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = projectRoot.Close() }()
	for _, file := range plan.converted {
		if err := writeRootFileAtomic(projectRoot, file.path, []byte(file.content)); err != nil {
			return err
		}
	}
	if plan.agents.content != nil {
		if err := writeRootFileAtomic(projectRoot, scaffold.AgentsFile, plan.agents.content); err != nil {
			return err
		}
	}
	return writeRootFileAtomic(projectRoot, scaffold.AppFile, plan.app)
}

// tenancyMigration adds org_id to tables in adapter's dialect. Each
// direction checks its preconditions before it changes a column or a row,
// and changes nothing when one fails (MySQL commits every DDL statement at
// once, so a refusal half-way would leave a half-converted schema behind;
// PostgreSQL's down lifts row-level security first, in the same
// transaction, because FORCE hides every row from the owning migrator):
//
//   - up: at most one organization exists (the application's one, whatever
//     its slug), and one exists when any table holds rows (setup ran);
//   - down: at most one organization exists, and no row names another one
//     (compared NULL-safely): dropping org_id would merge organizations.
//
// Then up adds org_id (nullable), fills it with that organization's ID,
// requires it (SQLite: triggers, since SQLite cannot add NOT NULL to a
// column), makes it name a tenant (a foreign key; SQLite: the same
// triggers), indexes it with id, and on PostgreSQL adds the row-level
// security policy of an org-scoped table; with slug it renames the
// organization. Down reverts the schema and gives the organization back the
// hidden slug of a single-organization application.
func tenancyMigration(adapter string, tables []string, slug string) (up, down string) {
	q := func(name string) string { return quoteIdentifier(adapter, name) }
	tenants := "(SELECT COUNT(*) FROM forge_tenants)"
	organization := "(SELECT id FROM forge_tenants ORDER BY id LIMIT 1)"
	var anyRows []string
	var foreignRows []string
	for _, table := range tables {
		anyRows = append(anyRows, fmt.Sprintf("EXISTS (SELECT 1 FROM %s)", q(table)))
		var distinct string
		switch adapter {
		case "postgres":
			distinct = q("org_id") + " IS DISTINCT FROM " + organization
		case "mysql":
			distinct = "NOT (" + q("org_id") + " <=> " + organization + ")"
		default:
			distinct = q("org_id") + " IS NOT " + organization
		}
		foreignRows = append(foreignRows, fmt.Sprintf("(SELECT COUNT(*) FROM %s WHERE %s)", q(table), distinct))
	}

	var u, d strings.Builder
	u.WriteString("-- forge generate tenancy: the application's tables belong to organizations.\n")
	u.WriteString("-- Every row so far belongs to the application's one organization.\n")
	d.WriteString("-- Reverts forge generate tenancy's schema. The generated code that filters by\n-- org_id is restored with version control.\n")
	upChecks := []tenancyCheck{{
		name:    "forge_tenancy_needs_at_most_one_organization",
		message: "forge generate tenancy converts an application with one organization, and this database has several",
		failed:  tenants + " > 1",
	}}
	if len(anyRows) > 0 {
		upChecks = append(upChecks, tenancyCheck{
			name:    "forge_tenancy_rows_need_forge_setup_first",
			message: "the tables hold rows but no organization exists: run forge setup first, so the rows have an organization to belong to",
			failed:  tenants + " = 0 AND (" + strings.Join(anyRows, " OR ") + ")",
		})
	}
	downChecks := []tenancyCheck{{
		name:    "forge_tenancy_rollback_needs_one_organization",
		message: "rollback refused: more than one organization exists, and a single-organization application would let every one of them sign in to the first one's data. Remove the other organizations (and their rows) first",
		failed:  tenants + " > 1",
	}}
	if len(foreignRows) > 0 {
		downChecks = append(downChecks, tenancyCheck{
			name:    "forge_tenancy_rollback_rows_of_other_organizations",
			message: "rollback refused: rows belong to another organization than the application's one (or to none), and dropping org_id would merge them into it",
			failed:  strings.Join(foreignRows, " + ") + " > 0",
		})
	}
	u.WriteString(tenancyGuard(adapter, upChecks))
	if adapter == "postgres" {
		// The migrator owns the tables, and FORCE ROW LEVEL SECURITY applies
		// the policy to the owner too: with no forge.tenant_id set it hides
		// every row, so the down's row check would count none. Lift row-level
		// security first; PostgreSQL DDL is transactional, so a refusal rolls
		// this back with everything else. (The up's checks run before any
		// policy exists, and forge_tenants has none.)
		d.WriteString("\n-- Row-level security off first, or the checks below would see no rows.\n")
		for _, table := range tables {
			fmt.Fprintf(&d, "DROP POLICY %s ON %s;\n", q(table+"_org"), q(table))
			fmt.Fprintf(&d, "ALTER TABLE %s NO FORCE ROW LEVEL SECURITY;\nALTER TABLE %s DISABLE ROW LEVEL SECURITY;\n", q(table), q(table))
		}
	}
	d.WriteString(tenancyGuard(adapter, downChecks))

	for _, table := range tables {
		index := q(table + "_org_id_idx")
		foreignKey := q(table + "_org_id_fk")
		fmt.Fprintf(&u, "\n-- %s\n", table)
		switch adapter {
		case "postgres":
			fmt.Fprintf(&u, "ALTER TABLE %s ADD COLUMN %s uuid;\n", q(table), q("org_id"))
			fmt.Fprintf(&u, "UPDATE %s SET %s = %s;\n", q(table), q("org_id"), organization)
			fmt.Fprintf(&u, "ALTER TABLE %s ALTER COLUMN %s SET NOT NULL;\n", q(table), q("org_id"))
			fmt.Fprintf(&u, "ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES forge_tenants (id);\n", q(table), foreignKey, q("org_id"))
			fmt.Fprintf(&u, "CREATE INDEX %s ON %s (%s, %s);\n", index, q(table), q("org_id"), q("id"))
			u.WriteString(rowSecurity(table, q))
		case "mysql":
			fmt.Fprintf(&u, "ALTER TABLE %s ADD COLUMN %s %s NULL AFTER %s;\n", q(table), q("org_id"), mysqlOrgIDType, q("id"))
			fmt.Fprintf(&u, "UPDATE %s SET %s = %s;\n", q(table), q("org_id"), organization)
			fmt.Fprintf(&u, "ALTER TABLE %s MODIFY %s %s NOT NULL;\n", q(table), q("org_id"), mysqlOrgIDType)
			fmt.Fprintf(&u, "CREATE INDEX %s ON %s (%s, %s);\n", index, q(table), q("org_id"), q("id"))
			fmt.Fprintf(&u, "ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES forge_tenants (id);\n", q(table), foreignKey, q("org_id"))
		default: // sqlite
			fmt.Fprintf(&u, "ALTER TABLE %s ADD COLUMN %s TEXT;\n", q(table), q("org_id"))
			fmt.Fprintf(&u, "UPDATE %s SET %s = %s;\n", q(table), q("org_id"), organization)
			fmt.Fprintf(&u, "-- SQLite adds neither NOT NULL nor a foreign key to an existing column: these\n-- triggers refuse a row whose org_id is missing or names no organization.\n")
			for _, event := range []string{"INSERT", "UPDATE OF " + q("org_id")} {
				name := q(table + "_org_id_required_" + strings.ToLower(strings.Fields(event)[0]))
				fmt.Fprintf(&u, "CREATE TRIGGER %s BEFORE %s ON %s WHEN NEW.%s IS NULL OR NOT EXISTS (SELECT 1 FROM forge_tenants WHERE id = NEW.%s) BEGIN SELECT RAISE(ABORT, '%s.org_id must name an organization'); END;\n", name, event, q(table), q("org_id"), q("org_id"), table)
			}
			fmt.Fprintf(&u, "CREATE INDEX %s ON %s (%s, %s);\n", index, q(table), q("org_id"), q("id"))
		}

		fmt.Fprintf(&d, "\n-- %s\n", table)
		switch adapter {
		case "postgres":
			fmt.Fprintf(&d, "ALTER TABLE %s DROP CONSTRAINT %s;\n", q(table), foreignKey)
			fmt.Fprintf(&d, "DROP INDEX %s;\n", index)
		case "mysql":
			fmt.Fprintf(&d, "ALTER TABLE %s DROP FOREIGN KEY %s;\n", q(table), foreignKey)
			fmt.Fprintf(&d, "DROP INDEX %s ON %s;\n", index, q(table))
		default:
			fmt.Fprintf(&d, "DROP TRIGGER %s;\nDROP TRIGGER %s;\n", q(table+"_org_id_required_insert"), q(table+"_org_id_required_update"))
			fmt.Fprintf(&d, "DROP INDEX %s;\n", index)
		}
		fmt.Fprintf(&d, "ALTER TABLE %s DROP COLUMN %s;\n", q(table), q("org_id"))
	}
	if slug != "" {
		fmt.Fprintf(&u, "\n-- The organization's sign-in slug (at most one organization exists).\nUPDATE forge_tenants SET slug = '%s';\n", slug)
	}
	fmt.Fprintf(&d, "\n-- The one organization signs in under the hidden slug of a single-organization\n-- application again.\nUPDATE forge_tenants SET slug = '%s';\n", auth.DefaultTenantSlug)
	return u.String(), d.String()
}

// mysqlOrgIDType is org_id on MySQL and MariaDB: the type and collation of
// forge_tenants.id, which a foreign key needs.
const mysqlOrgIDType = "CHAR(36) CHARACTER SET ascii COLLATE ascii_bin"

// tenancyCheck is a precondition of a tenancy migration: failed is a
// boolean SQL expression that is true when it does not hold.
type tenancyCheck struct {
	name    string
	message string
	failed  string
}

// tenancyGuard runs checks before anything changes and stops the migration
// at the first one that fails. PostgreSQL raises the message from a DO
// block. MySQL and SQLite have no such statement in a script, so a row goes
// into a temporary table (it changes no schema and disappears with the
// session) whose CHECK constraints, named after the checks, refuse it.
func tenancyGuard(adapter string, checks []tenancyCheck) string {
	var b strings.Builder
	b.WriteString("\n-- Preconditions, checked before anything changes: a refusal leaves the\n-- database exactly as it was.\n")
	if adapter == "postgres" {
		b.WriteString("DO $forge_tenancy$\nBEGIN\n")
		for _, check := range checks {
			fmt.Fprintf(&b, "    IF %s THEN\n        RAISE EXCEPTION '%%', '%s';\n    END IF;\n", check.failed, strings.ReplaceAll(check.message, "'", "''"))
		}
		b.WriteString("END\n$forge_tenancy$;\n")
		return b.String()
	}
	table, drop := "temp.forge_tenancy_guard", "DROP TABLE IF EXISTS temp.forge_tenancy_guard;\n"
	create := "CREATE TEMP TABLE forge_tenancy_guard ("
	if adapter == "mysql" {
		table, drop = "forge_tenancy_guard", "DROP TEMPORARY TABLE IF EXISTS forge_tenancy_guard;\n"
		create = "CREATE TEMPORARY TABLE forge_tenancy_guard ("
	}
	for _, check := range checks {
		fmt.Fprintf(&b, "-- %s: %s.\n", check.name, check.message)
	}
	b.WriteString(drop + create + "\n")
	var columns, values []string
	for i, check := range checks {
		column := fmt.Sprintf("check_%d", i+1)
		columns = append(columns, column)
		values = append(values, fmt.Sprintf("CASE WHEN %s THEN 1 ELSE 0 END", check.failed))
		fmt.Fprintf(&b, "    %s INTEGER NOT NULL,\n", column)
	}
	for i, check := range checks {
		separator := ","
		if i == len(checks)-1 {
			separator = ""
		}
		fmt.Fprintf(&b, "    CONSTRAINT %s CHECK (check_%d = 0)%s\n", check.name, i+1, separator)
	}
	b.WriteString(");\n")
	fmt.Fprintf(&b, "INSERT INTO %s (%s) SELECT %s;\n", table, strings.Join(columns, ", "), strings.Join(values, ", "))
	b.WriteString(drop)
	return b.String()
}
