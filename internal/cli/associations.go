package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
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
