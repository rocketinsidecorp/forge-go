package cli

import (
	"fmt"
	"strings"
)

// Code the templates of a resource with references fields share: test
// doubles for the referenced resources' repositories, and the in-memory
// nested lists. Rendering them here keeps the three test templates that need
// them (application, httpapi, htmlui) identical.

// ReferenceStubs declares, for each references field, a stand-in for the
// referenced resource's repository: every row exists but one, and List
// offers one.
func (v resourceView) ReferenceStubs() string {
	var out strings.Builder
	for _, reference := range v.References {
		fmt.Fprintf(&out, `
var (
	%[5]s = uuid.MustNew() // the one %[2]s that does not exist
	%[6]s   = uuid.MustNew() // the %[2]s List offers
)

// %[1]s stands in for the %[2]s repository the service checks %[3]s
// against. Only Get and List are called.
type %[1]s struct {
	application.%[4]sRepository
}

func (%[1]s) Get(_ context.Context, id uuid.UUID) (domain.%[4]s, error) {
	if id == %[5]s {
		return domain.%[4]s{}, domain.Err%[4]sNotFound
	}
	return domain.%[4]s{ID: id}, nil
}

func (%[1]s) List(_ context.Context, _ uuid.UUID, _ int) ([]domain.%[4]s, error) {
	return []domain.%[4]s{{ID: %[6]s}}, nil
}
`, reference.StubType(), reference.Human, reference.Field.Name, reference.Title, reference.MissingVar(), reference.KnownVar())
	}
	return out.String()
}

// MissingVar and KnownVar name the ids of ReferenceStubs.
func (r referenceView) MissingVar() string { return r.Child + "Missing" + r.Title }

// KnownVar is the id the stand-in's List offers.
func (r referenceView) KnownVar() string { return r.Child + "Known" + r.Title }

// StubType is the name of the stand-in ReferenceStubs declares for r.
func (r referenceView) StubType() string { return r.Child + r.PluralTitle + "Stub" }

// StubArgs are the stand-ins passed to New<Title>Service after the
// repository, each followed by a comma.
func (v resourceView) StubArgs() string {
	var out strings.Builder
	for _, reference := range v.References {
		fmt.Fprintf(&out, " %s{},", reference.StubType())
	}
	return out.String()
}

// MemoryListBy declares the ListBy<Parent> methods of the in-memory
// repository named receiver, filtering its List.
func (v resourceView) MemoryListBy(receiver string) string {
	var out strings.Builder
	for _, reference := range v.References {
		matches := "entity." + reference.Field.Title + " == " + reference.Camel + "ID"
		if reference.Field.Optional {
			matches = "entity." + reference.Field.Title + " != nil && *entity." + reference.Field.Title + " == " + reference.Camel + "ID"
		}
		fmt.Fprintf(&out, `
func (r *%[1]s) ListBy%[2]s(ctx context.Context, %[3]sID, after uuid.UUID, limit int) ([]domain.%[4]s, error) {
	// A negative limit lists every row.
	rows, err := r.List(ctx, after, -1)
	if err != nil {
		return nil, err
	}
	var page []domain.%[4]s
	for _, entity := range rows {
		if %[5]s && len(page) < limit {
			page = append(page, entity)
		}
	}
	return page, nil
}
`, receiver, reference.Title, reference.Camel, v.Title, matches)
	}
	return out.String()
}

// RepoUnderTest is the type the database tests hold the repository as: the
// repository itself, or, with references fields, the store that first
// inserts the rows they reference (ReferenceFixtures).
func (v resourceView) RepoUnderTest() string {
	if len(v.References) == 0 {
		return "*persistence." + v.Title + "Repository"
	}
	return lowerCamel(v.Name) + "Store"
}

// testDatabase is the type of the database handle the persistence tests get.
func (v resourceView) testDatabase() string {
	if v.Postgres {
		return "*postgrestest.RLS"
	}
	return "*sqldb.DB"
}

// ReferenceFixtures declares, for a resource with references fields, the
// store the database tests use (it inserts every referenced row, and the rows
// those reference in turn, before a write, so the foreign keys hold) and one
// helper per referenced table that inserts a row with the required columns.
func (v resourceView) ReferenceFixtures() string {
	if len(v.References) == 0 {
		return ""
	}
	child := lowerCamel(v.Name)
	var out strings.Builder
	fmt.Fprintf(&out, `
// %[1]sStore is the repository under test. Before every write it inserts the
// rows the %[2]s references%[3]s, so the foreign keys
// hold; the tests about references call the repository itself.
type %[1]sStore struct {
	*persistence.%[4]sRepository
	t        *testing.T
	database %[5]s
}

func (s %[1]sStore) Create(ctx context.Context, entity domain.%[4]s) (domain.%[4]s, error) {
	s.insertReferences(ctx, entity)
	return s.%[4]sRepository.Create(ctx, entity)
}

func (s %[1]sStore) Update(ctx context.Context, entity domain.%[4]s) (domain.%[4]s, error) {
	s.insertReferences(ctx, entity)
	return s.%[4]sRepository.Update(ctx, entity)
}

`, child, v.Name, map[bool]string{true: " in the context's organization", false: ""}[v.Tenant], v.Title, v.testDatabase())
	fmt.Fprintf(&out, "// insertReferences inserts the rows entity references, unless they exist.\nfunc (s %sStore) insertReferences(ctx context.Context, entity domain.%s) {\n\ts.t.Helper()\n", child, v.Title)
	if v.Tenant {
		out.WriteString("\ttenant, err := tenancy.Require(ctx)\n\tif err != nil {\n\t\treturn // the repository reports it\n\t}\n")
	} else {
		out.WriteString("\t_ = ctx\n")
	}
	for _, reference := range v.References {
		call := fmt.Sprintf("%sInsert%s(s.t, s.database, %s", child, reference.Title, map[bool]string{true: "tenant.ID, ", false: ""}[v.Tenant])
		if reference.Field.Optional {
			fmt.Fprintf(&out, "\tif entity.%[1]s != nil {\n\t\t%[2]s*entity.%[1]s)\n\t}\n", reference.Field.Title, call)
		} else {
			fmt.Fprintf(&out, "\t%sentity.%s)\n", call, reference.Field.Title)
		}
	}
	out.WriteString("}\n")
	out.WriteString(v.InsertHelpers())
	return out.String()
}

// InsertHelpers declares one helper per table v references, directly or
// through what it references, that inserts a row with the required columns:
// <child>Insert<Parent>(t, database, [orgID,] id).
func (v resourceView) InsertHelpers() string {
	child := lowerCamel(v.Name)
	var out strings.Builder
	written := map[string]bool{}
	var insert func(references []referenceView)
	insert = func(references []referenceView) {
		for _, reference := range references {
			name := child + "Insert" + reference.Title
			if written[name] {
				continue
			}
			written[name] = true
			out.WriteString(v.insertHelper(name, child, reference))
			insert(reference.Parent.References)
		}
	}
	insert(v.References)
	return out.String()
}

// insertHelper declares name, which inserts the row of reference's table with
// an id (in an organization, for an org-scoped table) unless it exists, and
// first the rows that row references.
func (v resourceView) insertHelper(name, child string, reference referenceView) string {
	parent := reference.Parent
	placeholder := func(n int) string {
		if v.Postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	columns := []string{parent.Q("id")}
	args := []string{"id"}
	if parent.Tenant {
		columns, args = append(columns, parent.Q("org_id")), append(args, "orgID")
	}
	var before strings.Builder
	for _, field := range parent.RequiredFields() {
		columns = append(columns, parent.Q(field.Name))
		value := sampleExpr(field, 1)
		if field.IsReference() {
			for _, grandparent := range parent.References {
				if grandparent.Field.Name == field.Name {
					variable := grandparent.Camel + "ID"
					fmt.Fprintf(&before, "\t%s := uuid.MustNew()\n\t%sInsert%s(t, database, %s%s)\n", variable, child, grandparent.Title, map[bool]string{true: "orgID, ", false: ""}[parent.Tenant], variable)
					value = variable
				}
			}
		}
		args = append(args, value)
	}
	columns = append(columns, parent.Q("version"), parent.Q("created_at"), parent.Q("updated_at"))
	args = append(args, "1", "now", "now")
	placeholders := make([]string, len(columns))
	for i := range columns {
		placeholders[i] = placeholder(i + 1)
	}
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", parent.Q(parent.Plural), strings.Join(columns, ", "), strings.Join(placeholders, ", "))
	existsSQL := fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s WHERE %s = %s)", parent.Q(parent.Plural), parent.Q("id"), placeholder(1))
	exec, query := "database.SQL().ExecContext", "database.SQL().QueryRowContext"
	if v.Postgres {
		// The test server's own user bypasses row-level security.
		exec, query = "database.Admin.Exec", "database.Admin.QueryRow"
	}
	var out strings.Builder
	fmt.Fprintf(&out, `
// %[1]s inserts the %[2]s with id%[3]s unless it exists.
func %[1]s(t *testing.T, database %[4]s%[5]s, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var exists bool
	if err := %[6]s(ctx, %[7]s, id).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		return
	}
%[8]s	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := %[9]s(ctx, %[10]s, %[11]s); err != nil {
		t.Fatal(err)
	}
}
`, name, reference.Human, map[bool]string{true: " in the organization orgID", false: ""}[parent.Tenant], v.testDatabase(), map[bool]string{true: ", orgID", false: ""}[parent.Tenant],
		query, goStringLiteral(existsSQL), before.String(), exec, goStringLiteral(insertSQL), strings.Join(args, ", "))
	return out.String()
}

// ReferenceErrorWant is the error a write the foreign key on r refuses is
// reported as: the field's own error, except on SQLite with several
// references fields, whose foreign key errors do not say which one failed
// (the violation is then reported as it is).
func (v resourceView) ReferenceErrorWant(r referenceView) string {
	if v.SQLite && len(v.References) > 1 {
		return "sqldb.ErrReferenceViolation"
	}
	return "domain.Err" + v.Title + r.Title + "NotFound"
}
