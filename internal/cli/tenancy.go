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
	if len(plan.tables) > 0 {
		fmt.Fprintf(&out, "\n  Org-scoped now: %s (an org_id column every query is filtered by%s).\n", strings.Join(plan.tables, ", "), map[bool]string{true: ", behind row-level security", false: ""}[plan.adapter == "postgres"])
	}
	for _, table := range plan.kept {
		fmt.Fprintf(&out, "\n  %s stays global: it was generated with --public, which an org-scoped table cannot be (an anonymous request names no organization). Destroy it and generate it again with --org-scoped to scope it.\n", table)
	}
	organization := auth.DefaultTenantSlug
	if slug != "" {
		organization = slug
	}
	fmt.Fprintf(&out, "\n  The application now has organizations: sign-in asks for one, setup takes --org, and generators need --org-scoped or --global. Existing users and rows belong to the organization %q.\n", organization)
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

	commands, err := recordedCommands(projectRoot)
	if err != nil {
		return tenancyPlan{}, err
	}
	for _, command := range commands {
		request, extend, err := parseRecordedCommand(command)
		if err != nil {
			return tenancyPlan{}, err
		}
		if request.Public {
			plan.kept = append(plan.kept, request.Plural)
			continue
		}
		if request.TenantScoped {
			continue
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
	paths := []string{scaffold.AppFile}
	for _, file := range append(slices.Clone(plan.migrations), plan.converted...) {
		paths = append(paths, file.path)
	}
	if err := rejectExistingSymlinkPaths(projectRoot, paths); err != nil {
		return tenancyPlan{}, err
	}
	return plan, nil
}

// recordedCommands are the generator commands the create migrations of
// db/migrations record, in migration order.
func recordedCommands(projectRoot *os.Root) ([]string, error) {
	dir := filepath.Join("db", "migrations")
	entries, err := fs.ReadDir(projectRoot.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var commands []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		source, err := projectRoot.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if match := recordedCommand.FindSubmatch(source); match != nil {
			commands = append(commands, string(match[1]))
		}
	}
	return commands, nil
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
	return writeRootFileAtomic(projectRoot, scaffold.AppFile, plan.app)
}

// tenancyMigration adds org_id to tables in adapter's dialect: added
// nullable (SQLite: empty), filled with the default organization's ID,
// required, indexed with id, and on PostgreSQL behind the row-level security
// policy of an org-scoped table. A database whose tables hold rows but has no
// default organization (setup never ran) fails the migration rather than
// leaving rows without one. The down reverts it and refuses, through a
// guard table's CHECK constraint, while any row belongs to another
// organization: dropping org_id would merge them into one.
func tenancyMigration(adapter string, tables []string, slug string) (up, down string) {
	q := func(name string) string { return quoteIdentifier(adapter, name) }
	defaultOrg := "(SELECT id FROM forge_tenants WHERE slug = '" + auth.DefaultTenantSlug + "')"
	var u, d strings.Builder
	u.WriteString("-- forge generate tenancy: the application's tables belong to organizations.\n")
	u.WriteString("-- Every row so far belongs to the one organization setup created.\n")
	if len(tables) == 0 {
		u.WriteString("-- No generated table needs an org_id column.\n")
	}
	d.WriteString("-- Reverts forge generate tenancy's schema. The generated code that filters by\n-- org_id is restored with version control.\n")
	if slug != "" {
		fmt.Fprintf(&d, "UPDATE forge_tenants SET slug = '%s' WHERE slug = '%s';\n", auth.DefaultTenantSlug, slug)
	}
	if len(tables) > 0 {
		d.WriteString("\n-- Refused while a row belongs to another organization than the first.\n")
		d.WriteString("DROP TABLE IF EXISTS forge_tenancy_rollback_guard;\n")
		d.WriteString("CREATE TABLE forge_tenancy_rollback_guard (rows_of_other_organizations INTEGER NOT NULL CHECK (rows_of_other_organizations = 0));\n")
	}
	for _, table := range tables {
		index := q(table + "_org_id_idx")
		fmt.Fprintf(&u, "\n-- %s\n", table)
		switch adapter {
		case "postgres":
			fmt.Fprintf(&u, "ALTER TABLE %s ADD COLUMN %s uuid;\n", q(table), q("org_id"))
			fmt.Fprintf(&u, "UPDATE %s SET %s = %s;\n", q(table), q("org_id"), defaultOrg)
			fmt.Fprintf(&u, "ALTER TABLE %s ALTER COLUMN %s SET NOT NULL;\n", q(table), q("org_id"))
			fmt.Fprintf(&u, "CREATE INDEX %s ON %s (%s, %s);\n", index, q(table), q("org_id"), q("id"))
			u.WriteString(rowSecurity(table, q))
		case "mysql":
			fmt.Fprintf(&u, "ALTER TABLE %s ADD COLUMN %s CHAR(36) NULL AFTER %s;\n", q(table), q("org_id"), q("id"))
			fmt.Fprintf(&u, "UPDATE %s SET %s = %s;\n", q(table), q("org_id"), defaultOrg)
			fmt.Fprintf(&u, "ALTER TABLE %s MODIFY %s CHAR(36) NOT NULL;\n", q(table), q("org_id"))
			fmt.Fprintf(&u, "CREATE INDEX %s ON %s (%s, %s);\n", index, q(table), q("org_id"), q("id"))
		default: // sqlite: a column added NOT NULL needs a constant default.
			fmt.Fprintf(&u, "ALTER TABLE %s ADD COLUMN %s TEXT NOT NULL DEFAULT '';\n", q(table), q("org_id"))
			fmt.Fprintf(&u, "UPDATE %s SET %s = %s;\n", q(table), q("org_id"), defaultOrg)
			fmt.Fprintf(&u, "-- The empty default never names an organization: refuse it.\n")
			for _, event := range []string{"INSERT", "UPDATE OF " + q("org_id")} {
				name := q(table + "_org_id_required_" + strings.ToLower(strings.Fields(event)[0]))
				fmt.Fprintf(&u, "CREATE TRIGGER %s BEFORE %s ON %s WHEN NEW.%s = '' BEGIN SELECT RAISE(ABORT, '%s.org_id is required'); END;\n", name, event, q(table), q("org_id"), table)
			}
			fmt.Fprintf(&u, "CREATE INDEX %s ON %s (%s, %s);\n", index, q(table), q("org_id"), q("id"))
		}

		fmt.Fprintf(&d, "\n-- %s\n", table)
		if adapter == "postgres" {
			fmt.Fprintf(&d, "DROP POLICY %s ON %s;\n", q(table+"_org"), q(table))
			fmt.Fprintf(&d, "ALTER TABLE %s NO FORCE ROW LEVEL SECURITY;\nALTER TABLE %s DISABLE ROW LEVEL SECURITY;\n", q(table), q(table))
		}
		fmt.Fprintf(&d, "INSERT INTO forge_tenancy_rollback_guard SELECT COUNT(*) FROM %s WHERE %s <> %s;\n", q(table), q("org_id"), defaultOrg)
		switch adapter {
		case "mysql":
			fmt.Fprintf(&d, "DROP INDEX %s ON %s;\n", index, q(table))
		case "sqlite":
			fmt.Fprintf(&d, "DROP TRIGGER %s;\nDROP TRIGGER %s;\n", q(table+"_org_id_required_insert"), q(table+"_org_id_required_update"))
			fmt.Fprintf(&d, "DROP INDEX %s;\n", index)
		default:
			fmt.Fprintf(&d, "DROP INDEX %s;\n", index)
		}
		fmt.Fprintf(&d, "ALTER TABLE %s DROP COLUMN %s;\n", q(table), q("org_id"))
	}
	if len(tables) > 0 {
		d.WriteString("\nDROP TABLE forge_tenancy_rollback_guard;\n")
	}
	if slug != "" {
		fmt.Fprintf(&u, "\n-- The first organization's sign-in slug.\nUPDATE forge_tenants SET slug = '%s' WHERE slug = '%s';\n", slug, auth.DefaultTenantSlug)
	}
	return u.String(), d.String()
}
