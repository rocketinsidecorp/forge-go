package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// TestFieldTypesFollowD22 is decision D22: the canonical field types and
// their aliases are accepted everywhere a field is parsed, recorded under
// their canonical names, and anything else is refused with the valid types.
func TestFieldTypesFollowD22(t *testing.T) {
	t.Parallel()
	for declaration, want := range map[string]resourceField{
		"title:string":       {Name: "title", Type: "string"},
		"body:text?":         {Name: "body", Type: "text", Optional: true},
		"count:integer":      {Name: "count", Type: "int"},
		"count:int":          {Name: "count", Type: "int"},
		"done:boolean":       {Name: "done", Type: "bool"},
		"done:bool":          {Name: "done", Type: "bool"},
		"at:timestamp":       {Name: "at", Type: "time"},
		"at:time":            {Name: "at", Type: "time"},
		"at:datetime?":       {Name: "at", Type: "time", Optional: true},
		"key:uuid":           {Name: "key", Type: "uuid"},
		"post:references":    {Name: "post_id", Type: "uuid", References: "post"},
		"post:belongs_to":    {Name: "post_id", Type: "uuid", References: "post"},
		"author:references?": {Name: "author_id", Type: "uuid", Optional: true, References: "author"},
	} {
		got, err := parseResourceField(declaration)
		if err != nil || got != want {
			t.Errorf("%s = %+v, %v; want %+v", declaration, got, err, want)
		}
	}
	for declaration, wantErr := range map[string]string{
		"price:float":       "string, text, integer, boolean, timestamp, uuid, references (also int, bool, time, datetime, belongs_to)",
		"price":             "must be written name:type",
		"org:references":    `field name "org_id" is reserved`,
		"tenant:belongs_to": `field name "tenant_id" is reserved`,
		"Post:references":   "must be snake_case",
		"posts:references":  "",
		"string:references": "Go keyword",
		"id:string":         "is reserved",
	} {
		_, err := parseResourceField(declaration)
		if wantErr == "" {
			continue
		}
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: err = %v, want %q", declaration, err, wantErr)
		}
	}
	if _, err := parseField("post:references", false); err == nil || !strings.Contains(err.Error(), "post_id:uuid") {
		t.Errorf("a job payload with a references field: %v", err)
	}
	if field, err := parseField("count:integer", false); err != nil || field.Type != "int" {
		t.Errorf("a job payload with an integer: %+v, %v", field, err)
	}

	request, err := parseGeneratorArgs([]string{"comment", "body:text", "count:int", "seen:bool", "at:datetime?", "post:belongs_to", "--on-delete=cascade", "--org-scoped"}, scaffoldUsage)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := generatorCommand(request), "resource comment body:text count:integer seen:boolean at:timestamp? post:references --plural comments --on-delete cascade --org-scoped"; got != want {
		t.Errorf("recorded command = %q, want %q", got, want)
	}
	for args, wantErr := range map[string]string{
		"comment body:text --on-delete=cascade":                            "--on-delete is for resources with a references field",
		"comment post:references --on-delete=nullify":                      "--on-delete takes restrict",
		"comment post:references --on-delete cascade --on-delete=restrict": "--on-delete given more than once",
		"comment post:references post_id:uuid":                             `field "post_id" is declared twice`,
	} {
		if _, err := parseGeneratorArgs(strings.Fields(args), scaffoldUsage); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: err = %v, want %q", args, err, wantErr)
		}
	}
	if request, err := parseGeneratorArgs([]string{"comment", "post:references", "--on-delete", "restrict"}, scaffoldUsage); err != nil || request.Cascade {
		t.Errorf("--on-delete restrict: %+v, %v", request, err)
	}

	dir := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	generateIn(t, dir, "scaffold", "post", "title:string")
	if _, err := generate(dir, "migration", "add_post_to_notes", "post:references?"); err == nil || !strings.Contains(err.Error(), "forge generate migration does not write") {
		t.Errorf("a references field in forge generate migration: %v", err)
	}
	generateIn(t, dir, "migration", "add_count_to_posts", "count:integer", "flag:boolean", "at:timestamp?")
}

// TestAssociationsInASingleOrganizationApplication is decision D23 in an
// application with one organization: plain foreign keys, RESTRICT unless
// --on-delete=cascade, the parent's page gains the children's frames and
// loses them again, and what is still referenced is never destroyed.
func TestAssociationsInASingleOrganizationApplication(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	if _, err := generate(dir, "scaffold", "comment", "body:text", "post:references"); err == nil || !strings.Contains(err.Error(), "needs the post resource") {
		t.Fatalf("a reference to a missing resource: %v", err)
	}
	generateIn(t, dir, "scaffold", "post", "title:string", "body:text")
	associations := filepath.Join(dir, "app", "views", "posts", "_associations.gohtml")
	empty := readFile(t, associations)
	if strings.Contains(empty, "turbo-frame") || !strings.Contains(readFile(t, filepath.Join(dir, "app", "views", "posts", "show.gohtml")), `{{template "posts/_associations" .}}`) {
		t.Fatalf("a scaffold's show page includes its associations partial, empty at first:\n%s", empty)
	}

	output := generateIn(t, dir, "scaffold", "comment", "body:text", "post:references")
	if !strings.Contains(output, "update  app/views/posts/_associations.gohtml") {
		t.Errorf("the post page was not given the comments frame:\n%s", output)
	}
	up := readFile(t, firstMatch(t, dir, "*_create_comments.up.sql"))
	for _, want := range []string{
		"-- forge:generate scaffold comment body:text post:references --plural comments\n",
		`CONSTRAINT "comments_post_id_fk" FOREIGN KEY ("post_id") REFERENCES "posts" ("id") ON DELETE RESTRICT`,
		`CREATE INDEX "comments_post_id_idx" ON "comments" ("post_id");`,
	} {
		if !strings.Contains(up, want) {
			t.Errorf("the comments migration lacks %q:\n%s", want, up)
		}
	}
	if strings.Contains(up, "UNIQUE INDEX") {
		t.Errorf("a single-organization table needs no (org_id, id) key:\n%s", up)
	}
	frames := readFile(t, associations)
	if !strings.Contains(frames, `<turbo-frame id="post_comments" src="/posts/{{.Item.ID}}/comments" target="_top">`) {
		t.Errorf("the post page has no comments frame:\n%s", frames)
	}
	for _, path := range []string{"app/views/comments/by_post.gohtml", "app/views/comments/_associations.gohtml"} {
		if !exists(dir, path) {
			t.Errorf("%s was not generated", path)
		}
	}
	routes := readFile(t, filepath.Join(dir, "app", "bootstrap", "routes_gen.go"))
	if !strings.Contains(routes, "application.NewCommentService(persistence.NewCommentRepository(database), persistence.NewPostRepository(database), time.Now, uuid.New)") {
		t.Errorf("the comments block does not give the service the posts repository:\n%s", routes)
	}
	// Running it again changes nothing.
	again := generateIn(t, dir, "scaffold", "comment", "body:text", "post:references")
	if strings.Contains(again, "create  ") || strings.Contains(again, "update  ") {
		t.Errorf("a second run changed files:\n%s", again)
	}

	generateIn(t, dir, "scaffold", "note", "body:text", "post:references", "--on-delete=cascade")
	if up := readFile(t, firstMatch(t, dir, "*_create_notes.up.sql")); !strings.Contains(up, `REFERENCES "posts" ("id") ON DELETE CASCADE`) || !strings.Contains(up, "--on-delete cascade") {
		t.Errorf("--on-delete=cascade:\n%s", up)
	}
	generateIn(t, dir, "model", "badge", "label:string", "post:references")
	generateIn(t, dir, "scaffold", "tag", "name:string")
	if _, err := generate(dir, "join", "post", "badge"); err == nil || !strings.Contains(err.Error(), "badge is a model") {
		t.Errorf("a join with a model: %v", err)
	}
	output = generateIn(t, dir, "join", "post", "tag")
	for _, want := range []string{"update  app/views/posts/_associations.gohtml", "create  app/adapters/htmlui/post_tags.go", "wire  app/bootstrap/routes_gen.go"} {
		if !strings.Contains(output, want) {
			t.Errorf("forge generate join post tag did not %q:\n%s", want, output)
		}
	}
	join := readFile(t, firstMatch(t, dir, "*_create_post_tags.up.sql"))
	for _, want := range []string{
		"-- forge:generate join post tag\n",
		`PRIMARY KEY ("post_id", "tag_id")`,
		`CONSTRAINT "post_tags_post_id_fk" FOREIGN KEY ("post_id") REFERENCES "posts" ("id") ON DELETE CASCADE`,
		`CONSTRAINT "post_tags_tag_id_fk" FOREIGN KEY ("tag_id") REFERENCES "tags" ("id") ON DELETE CASCADE`,
		`CREATE INDEX "post_tags_tag_id_idx" ON "post_tags" ("tag_id");`,
	} {
		if !strings.Contains(join, want) {
			t.Errorf("the join migration lacks %q:\n%s", want, join)
		}
	}
	if !strings.Contains(readFile(t, associations), `<turbo-frame id="post_tags" src="/posts/{{.Item.ID}}/tags">`) {
		t.Errorf("the post page has no tags frame:\n%s", readFile(t, associations))
	}
	if again := generateIn(t, dir, "join", "post", "tag"); strings.Contains(again, "create  ") {
		t.Errorf("a second join run created files:\n%s", again)
	}
	generatedGoParses(t, dir)

	// What is referenced is never destroyed, nor converted to organizations.
	for _, args := range [][]string{
		{"scaffold", "post", "title:string", "body:text"},
		{"scaffold", "tag", "name:string"},
	} {
		if _, err := destroy(dir, args...); err == nil || !strings.Contains(err.Error(), "still reference it") {
			t.Errorf("destroy %v: %v", args, err)
		}
	}
	if _, err := runTenancy(dir); err == nil || !strings.Contains(err.Error(), "the application has associations") {
		t.Errorf("forge generate tenancy with associations: %v", err)
	}

	// destroy join takes the frame out again.
	if _, err := destroy(dir, "join", "post", "tag"); err != nil {
		t.Fatal(err)
	}
	if exists(dir, "app/adapters/htmlui/post_tags.go") || strings.Contains(readFile(t, associations), "post_tags") {
		t.Errorf("destroy join left its page or frame:\n%s", readFile(t, associations))
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "app", "bootstrap", "routes_gen.go")), "forge:join") {
		t.Error("destroy join left its block in the seam")
	}

	// An edited parent partial is refused by the generators, and kept by
	// destroy --force.
	writeFile(t, associations, readFile(t, associations)+"<p>mine</p>\n")
	before := snapshotTree(t, dir)
	if _, err := generate(dir, "scaffold", "reply", "body:text", "post:references"); err == nil || !strings.Contains(err.Error(), "it is not what the generators wrote") {
		t.Errorf("a child scaffold with an edited partial: %v", err)
	}
	// The join's migrations were kept: generating it again is refused
	// until they are rolled back and removed.
	if _, err := generate(dir, "join", "post", "tag"); err == nil || !strings.Contains(err.Error(), "already creates the table post_tags") {
		t.Errorf("join over its kept migration: %v", err)
	}
	if _, err := destroy(dir, "scaffold", "comment", "body:text", "post:references"); err == nil || !strings.Contains(err.Error(), "it is not what the generators wrote") {
		t.Errorf("destroy with an edited partial: %v", err)
	}
	if !slices.Equal(before, snapshotTree(t, dir)) {
		t.Error("a refused run changed files")
	}
	output, err := destroy(dir, "scaffold", "comment", "body:text", "post:references", "--force")
	if err != nil || !strings.Contains(output, "remove the frame of this scaffold by hand") || !strings.Contains(readFile(t, associations), "<p>mine</p>") {
		t.Errorf("destroy --force: %v\n%s", err, output)
	}
}

// TestAssociationsWithOrganizations: the foreign keys are composite, the
// referenced table's (org_id, id) key is declared once, by the first
// migration that needs it, and scopes never mix.
func TestAssociationsWithOrganizations(t *testing.T) {
	t.Parallel()
	for _, database := range []string{"postgresql", "mysql", "sqlite"} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()
			dir := newApp(t, scaffold.Options{Database: database, Tenants: true})
			q := func(name string) string {
				if database == "mysql" {
					return "`" + name + "`"
				}
				return `"` + name + `"`
			}
			generateIn(t, dir, "scaffold", "post", "title:string", "--org-scoped")
			generateIn(t, dir, "scaffold", "category", "name:string", "--global")
			generateIn(t, dir, "scaffold", "comment", "body:text", "post:references", "--org-scoped")
			up := readFile(t, firstMatch(t, dir, "*_create_comments.up.sql"))
			down := readFile(t, firstMatch(t, dir, "*_create_comments.down.sql"))
			for _, want := range []string{
				"CREATE UNIQUE INDEX " + q("posts_org_id_id_key") + " ON " + q("posts") + " (" + q("org_id") + ", " + q("id") + ");",
				"CONSTRAINT " + q("comments_post_id_fk") + " FOREIGN KEY (" + q("org_id") + ", " + q("post_id") + ") REFERENCES " + q("posts") + " (" + q("org_id") + ", " + q("id") + ") ON DELETE RESTRICT",
			} {
				if !strings.Contains(up, want) {
					t.Errorf("the comments migration lacks %q:\n%s", want, up)
				}
			}
			if !strings.Contains(down, "DROP INDEX "+q("posts_org_id_id_key")) {
				t.Errorf("the down does not drop the key it declared:\n%s", down)
			}
			generateIn(t, dir, "scaffold", "note", "body:text", "post:references", "--org-scoped")
			if up := readFile(t, firstMatch(t, dir, "*_create_notes.up.sql")); strings.Contains(up, "UNIQUE INDEX") {
				t.Errorf("the key was declared twice:\n%s", up)
			}
			for args, wantErr := range map[string]string{
				"remark body:text post:references --global":        "a global row, read by every organization, cannot point at one organization's post",
				"entry body:text category:references --org-scoped": "an organization's entries may reference only rows of the same organization",
			} {
				if _, err := generate(dir, "scaffold", strings.Fields(args)...); err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Errorf("%s: %v, want %q", args, err, wantErr)
				}
			}
			if _, err := generate(dir, "join", "post", "category"); err == nil || !strings.Contains(err.Error(), "global") {
				t.Errorf("a join across scopes: %v", err)
			}
			generateIn(t, dir, "scaffold", "tag", "name:string", "--org-scoped")
			generateIn(t, dir, "join", "post", "tag")
			join := readFile(t, firstMatch(t, dir, "*_create_post_tags.up.sql"))
			for _, want := range []string{
				"PRIMARY KEY (" + q("org_id") + ", " + q("post_id") + ", " + q("tag_id") + ")",
				"CREATE UNIQUE INDEX " + q("tags_org_id_id_key"),
				"FOREIGN KEY (" + q("org_id") + ", " + q("tag_id") + ") REFERENCES " + q("tags") + " (" + q("org_id") + ", " + q("id") + ") ON DELETE CASCADE",
			} {
				if !strings.Contains(join, want) {
					t.Errorf("the join migration lacks %q:\n%s", want, join)
				}
			}
			if strings.Contains(join, "posts_org_id_id_key") {
				t.Errorf("the join declared the posts key again:\n%s", join)
			}
			if database == "postgresql" && !strings.Contains(join, `CREATE POLICY "post_tags_org" ON "post_tags"`) {
				t.Errorf("the join table has no row-level security:\n%s", join)
			}
			generatedGoParses(t, dir)
		})
	}
}

// TestJoinRefusals: what forge generate join refuses, writing nothing.
func TestJoinRefusals(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	generateIn(t, dir, "scaffold", "post", "title:string")
	generateIn(t, dir, "resource", "tag", "name:string")
	before := snapshotTree(t, dir)
	for args, wantErr := range map[string]string{
		"post":                         "usage: forge generate join",
		"post tag extra":               "usage: forge generate join",
		"post post":                    "joins two different resources",
		"post tag --force":             `unknown flag "--force"`,
		"post Tag":                     "must be singular snake_case",
		"post label":                   "needs the label resource",
		"post tag --dry-run --dry-run": `unknown flag "--dry-run"`,
	} {
		if _, err := generate(dir, "join", strings.Fields(args)...); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("join %s: %v, want %q", args, err, wantErr)
		}
	}
	if output, err := generate(dir, "join", "post", "tag", "--dry-run"); err != nil || !strings.Contains(output, "would create") {
		t.Errorf("--dry-run: %v\n%s", err, output)
	}
	if !slices.Equal(before, snapshotTree(t, dir)) {
		t.Error("a refused or dry run wrote files")
	}
	// The second side is JSON only: the join still has its pages on the
	// post's side.
	generateIn(t, dir, "join", "post", "tag")
	if !exists(dir, "app/views/post_tags/index.gohtml") {
		t.Error("the join of a scaffold has no page")
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "locales", "post_tags.en.json")); err != nil {
		t.Error(err)
	}
}

// TestJoinRefusesWhatItCannotJoin covers the joins refused before anything
// is written, and the printed wiring of an application without the seam.
func TestJoinRefusesWhatItCannotJoin(t *testing.T) {
	t.Parallel()
	plain := newApp(t, scaffold.Options{Database: "sqlite"})
	generateIn(t, plain, "scaffold", "post", "title:string", "--public")
	generateIn(t, plain, "scaffold", "tag", "name:string")
	generateIn(t, plain, "model", "badge", "label:string")
	generateIn(t, plain, "resource", "post_tag", "note:string", "--plural", "post_tag_rows")
	for args, wantErr := range map[string]string{
		"post tag":   "guarded differently",
		"badge post": "badge is a model",
		"post label": "needs the label resource",
	} {
		if _, err := generate(plain, "join", strings.Fields(args)...); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("join %s: %v, want %q", args, err, wantErr)
		}
	}
	generateIn(t, plain, "scaffold", "label", "name:string", "--public")
	if _, err := generate(plain, "join", "post", "label"); err != nil {
		t.Errorf("a join between two public scaffolds: %v", err)
	}
	if _, err := generate(plain, "scaffold", "comment", "body:text", "post:references", "--public"); err != nil {
		t.Errorf("a public child of a public post: %v", err)
	}
	collides := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	generateIn(t, collides, "resource", "post", "title:string")
	generateIn(t, collides, "resource", "tag", "name:string")
	generateIn(t, collides, "resource", "post_tag", "note:string", "--plural", "post_tag_rows")
	if _, err := generate(collides, "join", "post", "tag"); err == nil || !strings.Contains(err.Error(), "would collide") {
		t.Errorf("a join named like a resource: %v", err)
	}
	broken := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	writeFile(t, filepath.Join(broken, ".env.development"), "bad line\n")
	if _, err := generate(broken, "join", "post", "tag"); err == nil || !strings.Contains(err.Error(), "environment files could not be loaded") {
		t.Errorf("a broken environment: %v", err)
	}
	none := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	writeFile(t, filepath.Join(none, ".env.development"), "FORGE_DATABASE_URL=\n")
	if _, err := generate(none, "join", "post", "tag"); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Errorf("an application without a database: %v", err)
	}
	if _, err := generate(t.TempDir(), "join", "post", "tag"); err == nil || !strings.Contains(err.Error(), "go.mod not found") {
		t.Errorf("outside an application: %v", err)
	}

	// Without the routes seam the wiring is printed, with the posts
	// repository the comments service checks against.
	old := resourceProject(t, "postgres://u:p@localhost/db")
	mustGenerate(t, old, "resource", "post", "title:string", "--org-scoped")
	mustGenerate(t, old, "resource", "tag", "name:string", "--org-scoped")
	output := mustGenerate(t, old, "resource", "comment", "body:text", "post:references", "--org-scoped")
	if !strings.Contains(output, "application.NewCommentService(persistence.NewCommentRepository(database), persistence.NewPostRepository(database), time.Now, uuid.New)") {
		t.Errorf("the printed wiring does not pass the posts repository:\n%s", output)
	}
	output = mustGenerate(t, old, "join", "post", "tag")
	for _, want := range []string{"Forge did not edit app/bootstrap/bootstrap.go", "application.NewPostTagService(persistence.NewPostTagRepository(database), persistence.NewPostRepository(database), persistence.NewTagRepository(database), time.Now)"} {
		if !strings.Contains(output, want) {
			t.Errorf("the printed join wiring lacks %q:\n%s", want, output)
		}
	}
	output, err := destroy(old, "join", "post", "tag", "--dry-run")
	if err != nil || !strings.Contains(output, "would remove") {
		t.Errorf("destroy join --dry-run: %v\n%s", err, output)
	}
	if got := canonicalFieldType("unknown"); got != "unknown" {
		t.Errorf("canonicalFieldType(unknown) = %q", got)
	}
}

// TestNestedPageOfAResourceWithoutText names its rows by id, and a destroy
// whose parent partial is gone changes nothing else.
func TestNestedPageOfAResourceWithoutText(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	generateIn(t, dir, "scaffold", "post", "title:string")
	generateIn(t, dir, "scaffold", "vote", "count:integer", "post:references")
	if page := readFile(t, filepath.Join(dir, "app", "views", "votes", "by_post.gohtml")); !strings.Contains(page, "{{.ID}}</a>") {
		t.Errorf("the nested page does not name votes by id:\n%s", page)
	}
	if err := os.Remove(filepath.Join(dir, "app", "views", "posts", "_associations.gohtml")); err != nil {
		t.Fatal(err)
	}
	if _, err := destroy(dir, "scaffold", "vote", "count:integer", "post:references"); err != nil {
		t.Fatalf("destroy with the parent partial gone: %v", err)
	}
	if exists(dir, "app/views/posts/_associations.gohtml") {
		t.Error("destroy created the parent partial")
	}
}

// TestDestroyKeepsTheUniqueKeysOthersReferenceThrough is the review's P1:
// the migration that declares a parent's UNIQUE (org_id, id) is never
// removed while another association's composite foreign key references it
// (forge migrate would fail on PostgreSQL and MySQL, and SQLite refuse every
// delete from the parent).
func TestDestroyKeepsTheUniqueKeysOthersReferenceThrough(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "sqlite", Tenants: true})
	generateIn(t, dir, "scaffold", "post", "title:string", "--org-scoped")
	generateIn(t, dir, "scaffold", "comment", "body:text", "post:references", "--org-scoped")
	generateIn(t, dir, "scaffold", "tag", "name:string", "--org-scoped")
	generateIn(t, dir, "join", "post", "tag")
	generateIn(t, dir, "scaffold", "note", "body:text", "post:references?", "--on-delete=cascade", "--org-scoped")
	comment := []string{"scaffold", "comment", "body:text", "post:references", "--org-scoped", "--migrations"}
	before := snapshotTree(t, dir)
	_, err := destroy(dir, comment...)
	for _, want := range []string{"refusing to destroy with --migrations", "create_comments migration declares posts_org_id_id_key", "notes (post:references", "the join post tag"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("destroy comment --migrations: %v, want %q", err, want)
		}
	}
	if _, err := destroy(dir, append(comment, "--force")...); err == nil || !strings.Contains(err.Error(), "posts_org_id_id_key") {
		t.Fatalf("--force must not override it: %v", err)
	}
	if !slices.Equal(before, snapshotTree(t, dir)) {
		t.Fatal("a refused destroy changed files")
	}
	// Without --migrations the migration, and its key, stay.
	if _, err := destroy(dir, comment[:len(comment)-1]...); err != nil {
		t.Fatalf("destroy comment keeping its migration: %v", err)
	}
	if _, err := destroy(dir, comment...); err == nil || !strings.Contains(err.Error(), "posts_org_id_id_key") {
		t.Fatalf("removing the kept migration later: %v", err)
	}
	// Once nothing else references posts, the migration may go.
	if _, err := destroy(dir, "scaffold", "note", "body:text", "post:references?", "--on-delete=cascade", "--org-scoped", "--migrations"); err != nil {
		t.Fatal(err)
	}
	if _, err := destroy(dir, "join", "post", "tag", "--migrations"); err != nil {
		t.Fatalf("the join declares only tags' key, which nothing else references: %v", err)
	}
	if _, err := destroy(dir, comment...); err != nil {
		t.Fatalf("destroy comment --migrations with nothing else on posts: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "db", "migrations", "*_create_comments.*")); len(matches) != 0 {
		t.Fatalf("the comments migrations were kept: %v", matches)
	}
}

// The same for a join that declared the key: a later child relies on it.
func TestDestroyJoinKeepsTheUniqueKeyAChildReferencesThrough(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "postgresql", Tenants: true})
	generateIn(t, dir, "scaffold", "post", "title:string", "--org-scoped")
	generateIn(t, dir, "scaffold", "tag", "name:string", "--org-scoped")
	generateIn(t, dir, "join", "post", "tag")
	generateIn(t, dir, "scaffold", "comment", "body:text", "post:references", "--org-scoped")
	if up := readFile(t, firstMatch(t, dir, "*_create_comments.up.sql")); strings.Contains(up, "UNIQUE INDEX") {
		t.Fatalf("the comments migration declared the key the join declared:\n%s", up)
	}
	before := snapshotTree(t, dir)
	if _, err := destroy(dir, "join", "post", "tag", "--migrations"); err == nil || !strings.Contains(err.Error(), "create_post_tags migration declares posts_org_id_id_key and tags_org_id_id_key") || !strings.Contains(err.Error(), "comments (post:references") {
		t.Fatalf("destroy join --migrations: %v", err)
	}
	if !slices.Equal(before, snapshotTree(t, dir)) {
		t.Fatal("a refused destroy changed files")
	}
	if _, err := generate(dir, "join", "tag", "post"); err == nil || !strings.Contains(err.Error(), "already joins post and tag") {
		t.Fatalf("a join the other way round: %v", err)
	}
	if partial := readFile(t, filepath.Join(dir, "app", "views", "posts", "_associations.gohtml")); !strings.Contains(partial, `{{if can "comments:read"}}`) || !strings.Contains(partial, `{{if can "tags:read"}}`) {
		t.Fatalf("the frames are not gated on their pages' permissions:\n%s", partial)
	}
	if form := readFile(t, filepath.Join(dir, "app", "views", "comments", "_form.gohtml")); !strings.Contains(form, `{{- if can "posts:read"}}`) {
		t.Fatalf("the post select is not gated on posts:read:\n%s", form)
	}
}

// TestIdentifierBudgetCoversTheUniqueKey: PLURAL_org_id_id_key is the
// longest name a table gets, and it must fit PostgreSQL's 63 bytes.
func TestIdentifierBudgetCoversTheUniqueKey(t *testing.T) {
	t.Parallel()
	for length, ok := range map[int]bool{49: true, 50: false} {
		plural := strings.Repeat("p", length)
		err := validateIdentifierLengths(resourceRequest{Plural: plural})
		if (err == nil) != ok || len(uniqueKeyName(plural)) > 63 && err == nil {
			t.Errorf("a %d-character plural: %v", length, err)
		}
	}
}

// TestAssociationsRefuseWhatTheyCannotTrust: recorded headers that no longer
// parse, cycles, joins whose side is gone, and files where they should not
// be stop the generators before anything is written.
func TestAssociationsRefuseWhatTheyCannotTrust(t *testing.T) {
	t.Parallel()
	migration := func(t *testing.T, dir, name, header string) {
		t.Helper()
		writeFile(t, filepath.Join(dir, "db", "migrations", name+".up.sql"), header+"\nSELECT 1;\n")
		writeFile(t, filepath.Join(dir, "db", "migrations", name+".down.sql"), "SELECT 1;\n")
	}

	corrupt := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	generateIn(t, corrupt, "scaffold", "post", "title:string")
	generateIn(t, corrupt, "scaffold", "tag", "name:string")
	migration(t, corrupt, "20200101000000_create_things", "-- forge:generate scaffold Thing name:varchar --plural things")
	for _, args := range [][]string{
		{"scaffold", "comment", "body:text", "post:references"},
		{"join", "post", "tag"},
	} {
		if _, err := generate(corrupt, args[0], args[1:]...); err == nil || !strings.Contains(err.Error(), "no longer parses") {
			t.Errorf("%v with a corrupt header: %v", args, err)
		}
	}
	if _, err := destroy(corrupt, "scaffold", "post", "title:string"); err == nil || !strings.Contains(err.Error(), "no longer parses") {
		t.Errorf("destroy with a corrupt header: %v", err)
	}
	if _, err := runTenancy(corrupt); err == nil || !strings.Contains(err.Error(), "no longer parses") {
		t.Errorf("tenancy with a corrupt header: %v", err)
	}

	cycle := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	migration(t, cycle, "20200101000000_create_alphas", "-- forge:generate model alpha beta:references --plural alphas")
	migration(t, cycle, "20200101000001_create_betas", "-- forge:generate model beta alpha:references --plural betas")
	if _, err := generate(cycle, "model", "gamma", "alpha:references"); err == nil || !strings.Contains(err.Error(), "cannot reference itself") {
		t.Errorf("a cycle of references: %v", err)
	}

	gone := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	generateIn(t, gone, "scaffold", "post", "title:string")
	migration(t, gone, "20300101000000_create_post_widgets", "-- forge:generate join post widget")
	if _, err := generate(gone, "scaffold", "post", "title:string"); err != nil {
		t.Errorf("a post whose join names a resource that is gone: %v", err)
	}

	long := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	generateIn(t, long, "resource", "abcdefghijklmnopqrst", "name:string")
	generateIn(t, long, "resource", "uvwxyzabcdefghijklmn", "name:string")
	if _, err := generate(long, "join", "abcdefghijklmnopqrst", "uvwxyzabcdefghijklmn"); err == nil || !strings.Contains(err.Error(), "the join table") {
		t.Errorf("a join table name over 40 characters: %v", err)
	}

	files := newApp(t, scaffold.Options{Database: "sqlite", Auth: true})
	generateIn(t, files, "scaffold", "post", "title:string")
	generateIn(t, files, "scaffold", "tag", "name:string")
	writeFile(t, filepath.Join(files, "app", "domain", "post_tags.go"), "package domain\n")
	if _, err := generate(files, "join", "post", "tag"); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("a join over an existing file: %v", err)
	}
	if err := os.Remove(filepath.Join(files, "app", "domain", "post_tags.go")); err != nil {
		t.Fatal(err)
	}
	routes := filepath.Join(files, "app", "bootstrap", "routes_gen.go")
	original := readFile(t, routes)
	writeFile(t, routes, original+"\n// mine\n")
	if _, err := generate(files, "join", "post", "tag"); err == nil || !strings.Contains(err.Error(), "edited by hand") {
		t.Errorf("a join with an edited seam: %v", err)
	}
	writeFile(t, routes, original)
	partial := filepath.Join(files, "app", "views", "posts", "_associations.gohtml")
	writeFile(t, partial, "mine\n")
	if _, err := generate(files, "join", "post", "tag"); err == nil || !strings.Contains(err.Error(), "it is not what the generators wrote") {
		t.Errorf("a join with an edited partial: %v", err)
	}
	if err := os.Remove(partial); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(files, "go.mod"), partial); err != nil {
		t.Fatal(err)
	}
	if _, err := generate(files, "scaffold", "comment", "body:text", "post:references"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("a child with a symlinked partial: %v", err)
	}
	if err := os.Remove(partial); err != nil {
		t.Fatal(err)
	}
	// An application whose parent page predates the partial gets it
	// created.
	output := generateIn(t, files, "scaffold", "comment", "body:text", "post:references")
	if !strings.Contains(output, "update  app/views/posts/_associations.gohtml") || !strings.Contains(readFile(t, partial), "post_comments") {
		t.Errorf("the missing partial was not created:\n%s", output)
	}
	generateIn(t, files, "join", "post", "tag")
	join := firstMatch(t, files, "*_create_post_tags.up.sql")
	writeFile(t, join, readFile(t, join)+"-- mine\n")
	if _, err := generate(files, "join", "post", "tag"); err == nil || !strings.Contains(err.Error(), "already creates the table post_tags") {
		t.Errorf("a join over its edited migration: %v", err)
	}
	for args, wantErr := range map[string]string{
		"post":       "usage: forge generate join",
		"post badge": "needs the badge resource",
	} {
		if _, err := destroy(files, append([]string{"join"}, strings.Fields(args)...)...); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("destroy join %s: %v, want %q", args, err, wantErr)
		}
	}
	output, err := destroy(files, "scaffold", "comment", "body:text", "post:references", "--dry-run")
	if err != nil || !strings.Contains(output, "would update") {
		t.Errorf("destroy --dry-run of a child: %v\n%s", err, output)
	}
}

// TestJoinsAndChildrenOfModelsAndGlobalResources: a scaffold may reference a
// model (no frame to add), a join of global resources keeps its writes
// behind the platform guard, an API-only application gets no pages, and an
// application in another default locale gets the join's text in it too.
func TestJoinsAndChildrenOfModelsAndGlobalResources(t *testing.T) {
	t.Parallel()
	dir := newApp(t, scaffold.Options{Database: "postgresql", Tenants: true, Locale: "pt-BR"})
	generateIn(t, dir, "model", "author", "name:string", "--org-scoped")
	if output := generateIn(t, dir, "scaffold", "book", "title:string", "author:references", "--org-scoped"); strings.Contains(output, "update  ") {
		t.Errorf("a child of a model updated a partial:\n%s", output)
	}
	if _, err := destroy(dir, "model", "author", "name:string", "--org-scoped"); err == nil || !strings.Contains(err.Error(), "still reference it") {
		t.Errorf("destroy a referenced model: %v", err)
	}
	if _, err := destroy(dir, "model", "author", "name:string", "--org-scoped", "--migrations"); err == nil || !strings.Contains(err.Error(), "still reference it") {
		t.Errorf("destroy --migrations of a referenced model: %v", err)
	}
	generateIn(t, dir, "scaffold", "genre", "name:string", "--global")
	generateIn(t, dir, "scaffold", "rack", "name:string", "--global")
	output := generateIn(t, dir, "join", "genre", "rack")
	if !strings.Contains(output, "create  config/locales/genre_racks.pt-BR.json") {
		t.Errorf("the join's text in the default locale:\n%s", output)
	}
	routes := readFile(t, filepath.Join(dir, "app", "bootstrap", "routes_gen.go"))
	if !strings.Contains(routes, `if permission == "genres:write" {`) || !strings.Contains(routes, "return protectPlatformWrite(permission, next)") {
		t.Errorf("the global join's writes are not behind the platform guard:\n%s", routes)
	}
	generatedGoParses(t, dir)

	api := t.TempDir()
	if _, err := scaffold.Generate(scaffold.Options{Name: "ledger", Module: "example.com/ledger", Dir: api, Database: "sqlite", API: true}); err != nil {
		t.Fatal(err)
	}
	generateIn(t, api, "resource", "account", "name:string", "--public")
	generateIn(t, api, "resource", "entry", "amount:integer", "account:references", "--public")
	generateIn(t, api, "resource", "label", "name:string", "--public")
	generateIn(t, api, "join", "account", "label")
	if exists(api, "app/adapters/htmlui/account_labels.go") || !exists(api, "app/adapters/httpapi/account_labels.go") {
		t.Error("an API-only join has pages, or no JSON routes")
	}
}
