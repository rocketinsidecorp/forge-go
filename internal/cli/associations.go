package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Associations (decisions D22 and D23, ADR 0051). A references field
// (comment post:references) is a belongs_to: the uuid column post_id, an
// index and a foreign key to the referenced resource's table, composite
// (org_id, post_id) -> posts (org_id, id) in an org-scoped table so that no
// row can reference another organization's. The referenced resource has many:
// its pages show the referencing scaffold's rows (app/views/PLURAL/
// _associations.gohtml), and forge generate join adds a many-to-many table
// between two resources. Everything is derived from the forge:generate
// headers the create migrations record, so generating, destroying and
// converting stay exact.

// recordedResource is a model, resource or scaffold as its create migration
// records it.
type recordedResource struct {
	request resourceRequest
	file    string // slash-separated path of its create migration
}

// recordedResources parses the generator commands every create migration of
// db/migrations records, in migration order.
func recordedResources(projectRoot *os.Root) ([]recordedResource, error) {
	recorded, _, err := recordedCommands(projectRoot)
	if err != nil {
		return nil, err
	}
	resources := make([]recordedResource, 0, len(recorded))
	for _, migration := range recorded {
		request, _, err := parseRecordedCommand(migration.command)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", migration.file, err)
		}
		resources = append(resources, recordedResource{request: request, file: migration.file})
	}
	return resources, nil
}

// findRecorded returns the recorded resource named name.
func findRecorded(resources []recordedResource, name string) (recordedResource, bool) {
	for _, resource := range resources {
		if resource.request.Name == name {
			return resource, true
		}
	}
	return recordedResource{}, false
}

// referenceSpec is a references field resolved against the application.
type referenceSpec struct {
	field  resourceField
	parent resourceRequest
	// file is the parent's create migration.
	file string
	// addsKey reports that the child's migration declares the parent's
	// UNIQUE (org_id, id), which the composite foreign key needs and no
	// earlier migration declares.
	addsKey bool
	// parents are the parent's own references, for the fixtures of the
	// child's database tests (a comment needs a post, which needs a blog).
	parents []referenceSpec
}

// resolveReferences finds the resource every references field of spec names,
// checks that its scope matches, and works out whether the child's migration
// must declare the parent's unique key.
func resolveReferences(projectRoot *os.Root, spec resourceSpec) ([]referenceSpec, error) {
	if !hasReferences(spec.Fields) {
		return nil, nil
	}
	resources, err := recordedResources(projectRoot)
	if err != nil {
		return nil, err
	}
	references, err := resolveFields(resources, spec.resourceRequest, spec.Organizations, nil)
	if err != nil {
		return nil, err
	}
	if !spec.TenantScoped {
		return references, nil
	}
	for i := range references {
		declared, err := declaresUniqueKey(projectRoot, "create_"+spec.Plural, spec.Database, references[i].parent.Plural)
		if err != nil {
			return nil, err
		}
		references[i].addsKey = !declared
	}
	return references, nil
}

// hasReferences reports whether fields include a references field.
func hasReferences(fields []resourceField) bool {
	return slices.ContainsFunc(fields, func(field resourceField) bool { return field.References != "" })
}

// resolveFields resolves request's references fields, and theirs in turn.
// seen holds the resources being resolved, so an edited header that makes a
// cycle is refused instead of recursing forever.
func resolveFields(resources []recordedResource, request resourceRequest, organizations bool, seen []string) ([]referenceSpec, error) {
	seen = append(seen, request.Name)
	var references []referenceSpec
	for _, field := range request.Fields {
		if field.References == "" {
			continue
		}
		if slices.Contains(seen, field.References) {
			return nil, fmt.Errorf("%s:references: %s cannot reference itself, directly or through the resources it references; a tree of %s needs a hand-written migration", field.References, request.Name, request.Plural)
		}
		parent, found := findRecorded(resources, field.References)
		if !found {
			return nil, fmt.Errorf("%[1]s:references needs the %[1]s resource, and no migration in db/migrations records generating it (forge generate model, resource or scaffold %[1]s): generate %[1]s first, then %[2]s. A table created by a hand-written migration cannot be referenced", field.References, request.Name)
		}
		if organizations {
			if err := matchScopes(request, parent); err != nil {
				return nil, err
			}
		}
		grandparents, err := resolveFields(resources, parent.request, organizations, seen)
		if err != nil {
			return nil, err
		}
		references = append(references, referenceSpec{field: field, parent: parent.request, file: parent.file, parents: grandparents})
	}
	return references, nil
}

// matchScopes refuses a reference across scopes in an application with
// organizations: an org-scoped row may reference only rows of its own
// organization (the composite foreign key enforces it), and a global row,
// which every organization reads, may not point into one organization.
func matchScopes(child resourceRequest, parent recordedResource) error {
	switch {
	case child.TenantScoped && !parent.request.TenantScoped:
		return fmt.Errorf("%[1]s:references: %[2]s is --org-scoped, but %[1]s is global (%[3]s): an organization's %[4]s may reference only rows of the same organization, which the foreign key (org_id, %[1]s_id) -> %[5]s (org_id, id) enforces. Reference an org-scoped resource, or generate %[2]s --global", parent.request.Name, child.Name, parent.file, child.Plural, parent.request.Plural)
	case !child.TenantScoped && parent.request.TenantScoped:
		return fmt.Errorf("%[1]s:references: %[2]s is --global, but %[1]s is org-scoped (%[3]s): a global row, read by every organization, cannot point at one organization's %[1]s. Generate %[2]s --org-scoped", parent.request.Name, child.Name, parent.file)
	}
	return nil
}

// uniqueKeyName is the index that declares (org_id, id) unique on an
// org-scoped table, so composite foreign keys can reference it.
func uniqueKeyName(plural string) string { return plural + "_org_id_id_key" }

// declaresUniqueKey reports whether a migration of db/migrations other than
// the pair named own creates plural's (org_id, id) unique key.
func declaresUniqueKey(projectRoot *os.Root, own, database, plural string) (bool, error) {
	dir := filepath.Join("db", "migrations")
	entries, err := fs.ReadDir(projectRoot.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	statement := "CREATE UNIQUE INDEX " + quoteIdentifier(database, uniqueKeyName(plural)) + " "
	for _, entry := range entries {
		_, rest, ok := strings.Cut(entry.Name(), "_")
		if !ok || entry.IsDir() || !strings.HasSuffix(rest, ".up.sql") || rest == own+".up.sql" {
			continue
		}
		source, err := projectRoot.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return false, err
		}
		if strings.Contains(string(source), statement) {
			return true, nil
		}
	}
	return false, nil
}

// joinHeader finds the join a create migration records.
var joinHeader = regexp.MustCompile(`(?m)^-- forge:generate join ([a-z][a-z0-9_]*) ([a-z][a-z0-9_]*)$`)

// recordedJoin is a join as its create migration records it.
type recordedJoin struct {
	left, right string // post and tag for `forge generate join post tag`
	file        string
}

// recordedJoins lists the joins the migrations of db/migrations record, in
// migration order.
func recordedJoins(projectRoot *os.Root) ([]recordedJoin, error) {
	dir := filepath.Join("db", "migrations")
	entries, err := fs.ReadDir(projectRoot.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var joins []recordedJoin
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		source, err := projectRoot.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if match := joinHeader.FindSubmatch(source); match != nil {
			joins = append(joins, recordedJoin{left: string(match[1]), right: string(match[2]), file: filepath.ToSlash(filepath.Join(dir, entry.Name()))})
		}
	}
	return joins, nil
}

// dependents lists what references the resource named name: the resources
// with a references field to it and the joins that name it, each as "what
// (where)". forge destroy refuses to remove a resource that still has any.
func dependents(projectRoot *os.Root, name string) ([]string, error) {
	resources, err := recordedResources(projectRoot)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, resource := range resources {
		for _, field := range resource.request.Fields {
			if field.References == name && resource.request.Name != name {
				found = append(found, fmt.Sprintf("%s (%s:references, %s)", resource.request.Plural, name, resource.file))
			}
		}
	}
	joins, err := recordedJoins(projectRoot)
	if err != nil {
		return nil, err
	}
	for _, join := range joins {
		if join.left == name || join.right == name {
			found = append(found, fmt.Sprintf("the join %s %s (%s)", join.left, join.right, join.file))
		}
	}
	return found, nil
}

// associationEntry is one Turbo Frame of a scaffold's
// app/views/PLURAL/_associations.gohtml: the page of something that
// references the row its show page displays.
type associationEntry struct {
	// sort orders the frames: the create migration of what they show.
	sort string
	// frame is the frame's id, src its path (a template, with the row's id),
	// title the translation key of its fallback link.
	frame, src, title string
	// top makes the frame's links and forms navigate the whole page.
	top bool
	// permission is what the frame's page needs: without it the frame is
	// left out (the page would answer 403, and Turbo show "Content missing").
	permission string
}

// associationsPath is the partial a scaffold's show page includes.
func associationsPath(plural string) string {
	return filepath.Join("app", "views", plural, "_associations.gohtml")
}

// childEntry is the frame of a scaffold whose references field names parent.
func childEntry(parent resourceRequest, child resourceRequest, migration string) associationEntry {
	return associationEntry{
		sort:       path.Base(migration),
		frame:      parent.Name + "_" + child.Plural,
		src:        "/" + parent.Plural + "/{{.Item.ID}}/" + child.Plural,
		title:      child.Plural + ".by_" + parent.Name + ".title",
		top:        true,
		permission: child.Plural + ":read",
	}
}

// joinEntry is the frame of the join between left (parent) and right.
func joinEntry(left resourceRequest, right resourceRequest, migration string) associationEntry {
	return associationEntry{
		sort:       path.Base(migration),
		frame:      joinTable(left, right),
		src:        "/" + left.Plural + "/{{.Item.ID}}/" + right.Plural,
		title:      joinTable(left, right) + ".index.title",
		permission: right.Plural + ":read",
	}
}

// joinTable is the table of the join between left and right: post_tags.
func joinTable(left, right resourceRequest) string { return left.Name + "_" + right.Plural }

// associationEntries lists the frames of parent's page: the scaffolds that
// reference it and the joins it is the first side of, whose pages exist.
func associationEntries(projectRoot *os.Root, parent resourceRequest) ([]associationEntry, error) {
	resources, err := recordedResources(projectRoot)
	if err != nil {
		return nil, err
	}
	exists := func(file string) bool {
		_, err := projectRoot.Lstat(file)
		return err == nil
	}
	var entries []associationEntry
	for _, child := range resources {
		if !child.request.Scaffold || child.request.Name == parent.Name {
			continue
		}
		for _, field := range child.request.Fields {
			if field.References == parent.Name && exists(filepath.Join("app", "adapters", "htmlui", child.request.Name+".go")) {
				entries = append(entries, childEntry(parent, child.request, child.file))
			}
		}
	}
	joins, err := recordedJoins(projectRoot)
	if err != nil {
		return nil, err
	}
	for _, join := range joins {
		if join.left != parent.Name {
			continue
		}
		right, found := findRecorded(resources, join.right)
		if !found {
			continue
		}
		if exists(filepath.Join("app", "adapters", "htmlui", joinTable(parent, right.request)+".go")) {
			entries = append(entries, joinEntry(parent, right.request, join.file))
		}
	}
	return entries, nil
}

// renderAssociations is app/views/PLURAL/_associations.gohtml with entries.
func renderAssociations(parent resourceRequest, entries []associationEntry) string {
	slices.SortStableFunc(entries, func(a, b associationEntry) int { return strings.Compare(a.sort, b.sort) })
	var out strings.Builder
	fmt.Fprintf(&out, "{{/* forge:associations %s\n", parent.Plural)
	fmt.Fprintf(&out, "What references a %s, on its page (%s/show includes this partial). Each\n", strings.ReplaceAll(parent.Name, "_", " "), parent.Plural)
	out.WriteString("Turbo Frame loads the page that lists it, shown to callers who may read it.\n")
	out.WriteString("Generators rewrite this file when an association is generated or destroyed,\n")
	out.WriteString("and refuse when it was edited. */}}\n")
	if len(entries) == 0 {
		return out.String()
	}
	out.WriteString("<section class=\"associations\">\n")
	for _, entry := range entries {
		target := ""
		if entry.top {
			target = ` target="_top"`
		}
		fmt.Fprintf(&out, "{{if can %q}}\n<turbo-frame id=\"%s\" src=\"%s\"%s>\n\t<a href=\"%s\">{{t %q}}</a>\n</turbo-frame>\n{{end}}\n", entry.permission, entry.frame, entry.src, target, entry.src, entry.title)
	}
	out.WriteString("</section>\n")
	return out.String()
}

// fileRewrite changes a file another generation owns (a parent's
// _associations.gohtml): from what it must still be to what it becomes.
type fileRewrite struct {
	path     string
	from, to string
}

// checkRewrites decides each rewrite against the file as it is: one that
// already says to is dropped; a missing file is created when generating
// (create is true); anything else but from is refused, as an edited file.
func checkRewrites(projectRoot *os.Root, rewrites []fileRewrite, create bool) ([]fileRewrite, error) {
	var planned []fileRewrite
	for _, rewrite := range rewrites {
		if err := rejectExistingSymlinkPaths(projectRoot, []string{rewrite.path}); err != nil {
			return nil, err
		}
		current, err := projectRoot.ReadFile(rewrite.path)
		switch {
		case errors.Is(err, fs.ErrNotExist) && create:
			planned = append(planned, rewrite)
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, err
		case string(current) == rewrite.to:
		case string(current) == rewrite.from:
			planned = append(planned, rewrite)
		default:
			return nil, fmt.Errorf("refusing to change %s: it is not what the generators wrote (it was edited), so the association's frame cannot be added or removed safely; nothing was written. Restore it (git checkout %s), or move your changes into the show page, and run the command again", filepath.ToSlash(rewrite.path), filepath.ToSlash(rewrite.path))
		}
	}
	return planned, nil
}

// applyRewrites writes the planned rewrites.
func applyRewrites(root string, rewrites []fileRewrite) error {
	if len(rewrites) == 0 {
		return nil
	}
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = projectRoot.Close() }()
	for _, rewrite := range rewrites {
		if err := writeRootFileAtomic(projectRoot, rewrite.path, []byte(rewrite.to)); err != nil {
			return err
		}
	}
	return nil
}

// checkRewritesAt is checkRewrites for the application in root.
func checkRewritesAt(root string, rewrites []fileRewrite, create bool) ([]fileRewrite, error) {
	if len(rewrites) == 0 {
		return nil, nil
	}
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = projectRoot.Close() }()
	return checkRewrites(projectRoot, rewrites, create)
}

// reportRewrites lists the files rewrites change.
func reportRewrites(stdout io.Writer, rewrites []fileRewrite, dryRun bool) error {
	verb := "update"
	if dryRun {
		verb = "would update"
	}
	for _, rewrite := range rewrites {
		if _, err := fmt.Fprintf(stdout, "  %s  %s\n", verb, rewrite.path); err != nil {
			return err
		}
	}
	return nil
}

// parentRewrites are the changes a scaffold with references fields makes to
// the _associations.gohtml of every referenced scaffold: its frame added,
// or, for destroy, taken out.
func parentRewrites(projectRoot *os.Root, spec resourceSpec) ([]fileRewrite, error) {
	var rewrites []fileRewrite
	for _, reference := range spec.references {
		if !reference.parent.Scaffold {
			continue
		}
		natural, err := associationEntries(projectRoot, reference.parent)
		if err != nil {
			return nil, err
		}
		own := childEntry(reference.parent, spec.resourceRequest, "~") // a new migration sorts last
		var without []associationEntry
		present := false
		for _, entry := range natural {
			if entry.frame == own.frame {
				present = true
				continue
			}
			without = append(without, entry)
		}
		path := associationsPath(reference.parent.Plural)
		switch {
		case spec.Destroy && present:
			rewrites = append(rewrites, fileRewrite{path: path, from: renderAssociations(reference.parent, natural), to: renderAssociations(reference.parent, without)})
		case !spec.Destroy && !present:
			rewrites = append(rewrites, fileRewrite{path: path, from: renderAssociations(reference.parent, natural), to: renderAssociations(reference.parent, append(natural, own))})
		}
	}
	return rewrites, nil
}

// keyDependents lists what relies on the (org_id, id) unique keys that the
// migration of spec (a resource, or a join when join is set) declares: the
// other resources with a references field to the same table, and the joins
// that name it. Removing that migration would leave their composite foreign
// keys without the key they reference, so `forge destroy --migrations`
// refuses while any exists.
func keyDependents(projectRoot *os.Root, spec resourceSpec, join bool) ([]string, error) {
	var found []string
	for _, reference := range spec.references {
		if !reference.addsKey {
			continue
		}
		parent := reference.parent.Name
		resources, err := recordedResources(projectRoot)
		if err != nil {
			return nil, err
		}
		for _, resource := range resources {
			if !join && resource.request.Name == spec.Name {
				continue
			}
			for _, field := range resource.request.Fields {
				if field.References == parent {
					found = append(found, fmt.Sprintf("%s (%s:references, %s)", resource.request.Plural, parent, resource.file))
				}
			}
		}
		joins, err := recordedJoins(projectRoot)
		if err != nil {
			return nil, err
		}
		for _, other := range joins {
			self := join && other.left == spec.references[0].parent.Name && other.right == spec.references[1].parent.Name
			if !self && (other.left == parent || other.right == parent) {
				found = append(found, fmt.Sprintf("the join %s %s (%s)", other.left, other.right, other.file))
			}
		}
	}
	return found, nil
}

// keyDependentsRefusal is the reason `forge destroy --migrations` refuses to
// remove a migration that declares unique keys others rely on, or "".
func keyDependentsRefusal(root string, spec resourceSpec, join bool) (string, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer func() { _ = projectRoot.Close() }()
	found, err := keyDependents(projectRoot, spec, join)
	if err != nil || len(found) == 0 {
		return "", err
	}
	var keys []string
	for _, reference := range spec.references {
		if reference.addsKey {
			keys = append(keys, uniqueKeyName(reference.parent.Plural))
		}
	}
	return fmt.Sprintf("the %s migration declares %s, the unique key that the composite foreign keys of %s reference too: removing it would leave them without it (the next forge migrate would fail on PostgreSQL and MySQL, and SQLite would refuse every delete from the table). Destroy those first (with --migrations), or destroy this one without --migrations and keep its migration", "create_"+spec.Plural, strings.Join(keys, " and "), strings.Join(found, ", ")), nil
}
