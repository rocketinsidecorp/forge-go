package cli

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestGeneratedAssociationsBuildLintAndPass generates the owner's flow
// (decision D23: post, comment post:references, tag, join post tag, and a
// note that cascades) into an application on every database, with and
// without organizations, and requires it to vet, lint, pass the generated
// architecture test and the generated tests, the database ones against a
// real server: the foreign keys, the composite keys across organizations,
// RESTRICT and CASCADE.
func TestGeneratedAssociationsBuildLintAndPass(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	for _, variant := range []struct {
		name, database string
		flags          []string
		scope          []string
	}{
		{"sqlite --auth", "sqlite", []string{"--auth"}, nil},
		{"sqlite --tenants", "sqlite", []string{"--tenants"}, []string{"--org-scoped"}},
		{"mysql --auth", "mysql", []string{"--auth"}, nil},
		{"mysql --tenants", "mysql", []string{"--tenants"}, []string{"--org-scoped"}},
		{"postgresql --auth", "postgresql", []string{"--auth"}, nil},
		{"postgresql --tenants", "postgresql", []string{"--tenants"}, []string{"--org-scoped"}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			t.Parallel()
			with := func(args ...string) []string { return append(args, variant.scope...) }
			dir := matrixApplicationWith(t, variant.database, variant.flags, "scaffold", [][]string{
				with("post", "title:string", "body:text"),
				with("comment", "body:text", "post:references"),
				with("tag", "name:string"),
				with("note", "body:text", "post:references?", "--on-delete=cascade"),
				// A chain: a reply belongs to a comment, which belongs to a post.
				with("reply", "body:text", "comment:references", "post:references"),
			})
			var env []string
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "FORGE_DATABASE_URL=") {
					env = append(env, entry)
				}
			}
			var output bytes.Buffer
			if err := runGenerateEnv([]string{"join", "post", "tag"}, &output, dir, env, time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)); err != nil {
				t.Fatalf("generate join: %v\n%s", err, output.String())
			}
			goInMatrix(t, dir, "vet", "./...")
			packages := []string{"./app/", "./app/domain/...", "./app/application/...", "./app/adapters/htmlui/...", "./app/adapters/httpapi/..."}
			if databaseAvailable(variant.database) {
				packages = []string{"./app/..."}
			} else {
				t.Logf("no %s test server is configured; the database tests were skipped", variant.database)
			}
			command := exec.Command("go", append([]string{"test", "-count=1", "-v"}, packages...)...)
			command.Dir = dir
			command.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
			result, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("go test: %v\n%s", err, result)
			}
			want := []string{
				"--- PASS: TestArchitecture", "--- PASS: TestCommentServiceRefusesAMissingPost", "--- PASS: TestCommentBelongsPost",
				"--- PASS: TestCommentPagesBelongToPost", "--- PASS: TestPostTagPage", "--- PASS: TestPostTagRoutes",
				"--- PASS: TestReplyServiceListByComment",
			}
			if databaseAvailable(variant.database) {
				want = append(want, "--- PASS: TestCommentRepositoryRefusesAMissingPost", "--- PASS: TestCommentDeletingPostWithComments",
					"--- PASS: TestNoteDeletingPostWithNotes", "--- PASS: TestPostTagRowsGoWithEitherSide", "--- PASS: TestReplyRepositoryListByComment")
			}
			for _, test := range want {
				if !strings.Contains(string(result), test) {
					t.Errorf("the generated tests did not run %q", test)
				}
			}
			lint, err := exec.LookPath("golangci-lint")
			if err != nil {
				t.Log("golangci-lint is not on PATH; the lint check was skipped")
				return
			}
			golangciLint.Lock()
			defer golangciLint.Unlock()
			lintCommand := exec.Command(lint, "run", "--allow-parallel-runners", "./app/...")
			lintCommand.Dir = dir
			lintCommand.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
			if out, err := lintCommand.CombinedOutput(); err != nil {
				t.Fatalf("golangci-lint: %v\n%s", err, out)
			}
		})
	}
}
