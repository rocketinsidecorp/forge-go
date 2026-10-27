package forge_test

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/rocketinsidecorp/forge-go"

// nonCorePackages are the packages that will leave the core module when
// forge-go splits into modules (decision D38, ADR 0053), as import paths or
// "x/..." prefixes. Every other package of the module is core: it, its
// tests and everything they import may use only the standard library and
// other core packages, so an application that depends on the core alone
// links no third-party module, and the core module will need no require
// line (ADR 0006, ADR 0053).
var nonCorePackages = []string{
	// The database drivers' modules: the adapters, their test kits and the
	// tests of core packages that need a driver (*/internal/integration).
	modulePath + "/postgres/...",
	modulePath + "/mysql/...",
	modulePath + "/sqlite/...",
	modulePath + "/auth/postgres/...",
	modulePath + "/auth/mysql/...",
	modulePath + "/auth/sqlite/...",
	modulePath + "/queue/postgres/...",
	modulePath + "/queue/mysql/...",
	modulePath + "/queue/sqlite/...",
	modulePath + "/cache/postgres/...",
	modulePath + "/cache/mysql/...",
	modulePath + "/cache/sqlite/...",
	modulePath + "/ratelimit/postgres/...",
	modulePath + "/ratelimit/mysql/...",
	modulePath + "/ratelimit/sqlite/...",
	modulePath + "/jobs/postgres/...",
	modulePath + "/jobs/mysql/...",
	modulePath + "/broadcast/postgres/...",
	modulePath + "/outbox/postgres/...",
	modulePath + "/agent/postgres/...",
	// The tools module: the forge CLI and forge-proxy with what only they use
	// (esbuild, yaml, ACME, pgx for the db commands).
	modulePath + "/cmd/...",
	modulePath + "/internal/cli/...",
	modulePath + "/internal/scaffold/...",
	modulePath + "/internal/frontend/...",
	modulePath + "/internal/deploy/...",
	modulePath + "/internal/dbtasks/...",
	modulePath + "/internal/devloop/...",
	modulePath + "/internal/exectest/...",
	modulePath + "/internal/proxy/...",
	modulePath + "/internal/version/...",
	modulePath + "/internal/loadgen/...",
	// The example application, a module of its own.
	modulePath + "/examples/...",
}

// rootToolTests are the root package's repository-wide test files that use
// the forge CLI's packages or yaml (Rails parity, supply chain, release
// gate). They are the core's remaining non-core edges and move under cmd/
// with the tools (ADR 0053); until then the core rule reads the root
// package's other test files only. A file listed here must still need a
// non-core package, so the list cannot outlive its reason.
var rootToolTests = []string{
	"rails_parity_test.go",
	"release_ci_gate_test.go",
	"supply_chain_allowlist_negative_test.go",
	"supply_chain_allowlist_test.go",
	"supply_chain_rules_test.go",
	"supply_chain_test.go",
}

// isCore reports whether pkg, an import path, is a core package.
func isCore(pkg string) bool {
	return (pkg == modulePath || strings.HasPrefix(pkg, modulePath+"/")) && !matchesPath(pkg, nonCorePackages)
}

// isStandard reports whether pkg is in the standard library: its first path
// element has no dot (cmd/go's rule for the import paths a module can use).
func isStandard(pkg string) bool {
	return !strings.Contains(strings.SplitN(pkg, "/", 2)[0], ".")
}

// basePackage strips the test variant of a go list -test import path:
// "p [q.test]" is p compiled for q's tests.
func basePackage(pkg string) string {
	base, _, _ := strings.Cut(pkg, " [")
	return base
}

// coreDependencyViolations checks listing, the output of
// go list -deps -test -f '{{.ImportPath}}{{range .Imports}},{{.}}{{end}}'
// run on core packages: every import of a core package (or of the test
// main of one) must be the standard library or core. Only the first edge
// out of the core is reported; what lies beyond it is the same problem.
func coreDependencyViolations(listing string) []string {
	var problems []string
	seen := map[string]bool{}
	for line := range strings.Lines(listing) {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if fields[0] == "" {
			continue
		}
		importer := basePackage(fields[0])
		if !isCore(strings.TrimSuffix(importer, ".test")) {
			continue
		}
		for _, imported := range fields[1:] {
			dep := basePackage(imported)
			if isStandard(dep) || isCore(dep) || seen[importer+" "+dep] {
				continue
			}
			seen[importer+" "+dep] = true
			problems = append(problems, "core package "+importer+" (or its tests) imports "+dep+
				": core packages and their tests may import only the standard library and core packages (ADR 0053); move a test that needs a driver to <db>/internal/integration")
		}
	}
	return problems
}

// rootTestImports returns the imports of the root package's test files other
// than rootToolTests, and the rootToolTests that import nothing outside the
// standard library and core.
func rootTestImports(t *testing.T) (imports, staleToolTests []string) {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		tool, needsNonCore := slices.Contains(rootToolTests, file), false
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !isStandard(path) && !isCore(path) {
				needsNonCore = true
			}
			if !tool && !seen[path] {
				seen[path] = true
				imports = append(imports, path)
			}
		}
		if tool && !needsNonCore {
			staleToolTests = append(staleToolTests, file)
		}
	}
	for _, file := range rootToolTests {
		if !slices.Contains(files, file) {
			staleToolTests = append(staleToolTests, file)
		}
	}
	slices.Sort(imports)
	return imports, staleToolTests
}

// coreRuleGOOS are the operating systems the core rule lists packages for:
// a file with a _darwin or _windows suffix or build constraint is invisible
// to go list on Linux, and could import x/sys there.
var coreRuleGOOS = []string{"linux", "darwin", "windows"}

func TestCorePackagesAndTheirTestsDependOnlyOnStandardLibraryAndCore(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	t.Parallel()
	// The root package: its code and the test files other than
	// rootToolTests (its test imports, then what they import). Parsed
	// without build constraints, so every operating system's files count.
	imports, stale := rootTestImports(t)
	for _, file := range stale {
		t.Errorf("%s is in rootToolTests but is missing or needs no package outside the core: remove it from the list", file)
	}
	roots := []string{modulePath}
	for _, path := range imports {
		if !isStandard(path) {
			if !isCore(path) {
				t.Errorf("a root test file imports %s: only the files in rootToolTests may, until they move under cmd/ (ADR 0053)", path)
				continue
			}
			roots = append(roots, path)
		}
	}
	for _, goos := range coreRuleGOOS {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			goList := func(args ...string) string {
				t.Helper()
				command := exec.Command("go", append([]string{"list"}, args...)...)
				command.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64")
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("GOOS=%s go list %v: %v\n%s", goos, args, err, output)
				}
				return string(output)
			}
			var core []string
			for _, pkg := range strings.Fields(goList("./...")) {
				if isCore(pkg) && pkg != modulePath {
					core = append(core, pkg)
				}
			}
			if len(core) < 40 || !slices.Contains(core, modulePath+"/auth") || slices.Contains(core, modulePath+"/auth/postgres") {
				t.Fatalf("the core set looks wrong (%d packages): %v", len(core), core)
			}
			const format = "{{.ImportPath}}{{range .Imports}},{{.}}{{end}}"
			listing := goList(append([]string{"-deps", "-test", "-f", format}, core...)...)
			rootListing := goList(append([]string{"-deps", "-f", format}, roots...)...)
			for _, problem := range coreDependencyViolations(listing + rootListing) {
				t.Errorf("GOOS=%s: %s", goos, problem)
			}
		})
	}
}

func TestCoreDependencyViolationsFollowTestVariantsAndStopAtTheCore(t *testing.T) {
	t.Parallel()
	auth, sqldb := modulePath+"/auth", modulePath+"/sqldb"
	clean := strings.Join([]string{
		auth + ",errors," + modulePath + "/fault",
		auth + " [" + auth + ".test]," + modulePath + "/fault,testing",
		auth + ".test," + auth + " [" + auth + ".test],testing",
		sqldb + "_test [" + sqldb + ".test]," + sqldb + "," + modulePath + "/internal/waittest",
		// Beyond the core is not the core rule's business.
		modulePath + "/sqlite,modernc.org/sqlite",
		"",
	}, "\n")
	if problems := coreDependencyViolations(clean); len(problems) != 0 {
		t.Fatalf("a clean graph was refused: %v", problems)
	}
	for name, extra := range map[string]string{
		"auth imports x/crypto":           auth + ",golang.org/x/crypto/argon2",
		"a test variant imports a driver": sqldb + "_test [" + sqldb + ".test]," + modulePath + "/sqlite/sqlitetest",
		"a test imports a module":         sqldb + " [" + sqldb + ".test],modernc.org/sqlite",
		"core imports the CLI":            modulePath + "/views," + modulePath + "/internal/cli",
		"core imports an adapter":         modulePath + "/auth/authhttp," + modulePath + "/auth/postgres",
		"a test main imports a driver":    auth + ".test,github.com/jackc/pgx/v5",
	} {
		if problems := coreDependencyViolations(clean + extra + "\n"); len(problems) != 1 {
			t.Errorf("%s: problems %v, want exactly one", name, problems)
		}
	}
	for pkg, want := range map[string]bool{
		modulePath:                              true,
		modulePath + "/internal/terminal":       true,
		modulePath + "/postgres":                false,
		modulePath + "/postgres/internal/pgurl": false,
		modulePath + "/auth/sqlite":             false,
		modulePath + "/auth/authsql":            true,
		modulePath + "/internal/cli":            false,
		modulePath + "/cmd/forge":               false,
		modulePath + "-other":                   false,
		"golang.org/x/crypto/argon2":            false,
	} {
		if isCore(pkg) != want {
			t.Errorf("isCore(%s) = %v", pkg, !want)
		}
	}
}

// esbuildModule compiles the TypeScript front end of generated applications
// (ADR 0049). It is tooling: only the forge command line may link it, never a
// package that handles requests, and never an application built with Forge.
const esbuildModule = "github.com/evanw/esbuild"

// esbuildReachers are the only packages that may import esbuild, directly or
// through another package: internal/frontend wraps it, internal/scaffold
// compiles the TypeScript it generates, internal/cli runs the compiler for
// forge dev, build, check and test, and cmd/forge is the program.
var esbuildReachers = []string{
	modulePath + "/cmd/forge",
	modulePath + "/internal/cli",
	modulePath + "/internal/frontend",
	modulePath + "/internal/scaffold",
}

// esbuildReachingPackages returns the packages of listing, the output of
// go list -f '{{.ImportPath}}{{range .Imports}} {{.}}{{end}}' ./..., that
// import esbuild directly or through other listed packages, sorted.
func esbuildReachingPackages(listing string) []string {
	imports := map[string][]string{}
	for line := range strings.Lines(listing) {
		if fields := strings.Fields(line); len(fields) > 0 {
			imports[fields[0]] = fields[1:]
		}
	}
	reaches := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for pkg, imported := range imports {
			if reaches[pkg] {
				continue
			}
			for _, dep := range imported {
				if dep == esbuildModule || strings.HasPrefix(dep, esbuildModule+"/") || reaches[dep] {
					reaches[pkg], changed = true, true
					break
				}
			}
		}
	}
	var result []string
	for pkg := range reaches {
		result = append(result, pkg)
	}
	slices.Sort(result)
	return result
}

func TestEsbuildIsLinkedOnlyByTheForgeCommandLine(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	t.Parallel()
	output, err := exec.Command("go", "list", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, output)
	}
	got := esbuildReachingPackages(string(output))
	if !slices.Equal(got, esbuildReachers) {
		t.Fatalf("packages that link esbuild: %v\nonly %v may (ADR 0049): esbuild is build tooling and must not reach request handling, the framework's packages or the applications built with them", got, esbuildReachers)
	}
}

func TestEsbuildReachingPackagesFollowsImportsAndSortsThem(t *testing.T) {
	t.Parallel()
	listing := strings.Join([]string{
		modulePath + "/cmd/forge " + modulePath + "/internal/cli fmt",
		modulePath + "/internal/cli " + modulePath + "/internal/frontend",
		modulePath + "/internal/frontend " + esbuildModule + "/pkg/api",
		modulePath + "/web net/http",
		modulePath + "/middle " + modulePath + "/web " + modulePath + "/internal/frontend",
		"",
	}, "\n")
	want := []string{modulePath + "/cmd/forge", modulePath + "/internal/cli", modulePath + "/internal/frontend", modulePath + "/middle"}
	if got := esbuildReachingPackages(listing); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := esbuildReachingPackages(modulePath + "/web net/http\n"); len(got) != 0 {
		t.Errorf("a package that imports no esbuild was reported: %v", got)
	}
}

// importRule forbids packages from depending on others (ADR 0052). From
// lists import paths, or prefixes ending in "/..." for a path and everything
// under it; Forbidden likewise. Direct rules look only at the imports of
// From; the others follow imports through the module's packages.
type importRule struct {
	Name      string
	From      []string
	Forbidden []string
	Except    []string // packages of From the rule does not apply to
	Allowed   []string // packages of Forbidden that stay allowed
	Stop      []string // packages whose imports are not followed
	Direct    bool
}

// importRules are the dependency directions the framework keeps (ADR 0052).
var importRules = []importRule{
	{
		Name:      "auth is the domain: its HTTP adapter is auth/authhttp",
		From:      []string{modulePath + "/auth"},
		Forbidden: []string{modulePath + "/csrf", modulePath + "/i18n", modulePath + "/openapi", modulePath + "/web", modulePath + "/auth/authhttp"},
	},
	{
		// fault, which auth uses for its errors, maps them to HTTP statuses
		// and so links net/http; neither auth nor another framework package
		// it reaches may use net/http.
		Name:      "auth does not use net/http, directly or through the framework's packages (fault excepted)",
		From:      []string{modulePath + "/auth"},
		Forbidden: []string{"net/http", "net/http/..."},
		Stop:      []string{modulePath + "/fault"},
	},
	{
		// Writing the principal is how a request becomes authenticated, and
		// auth.AuthorizeMFAChange trusts it: only the middleware that
		// resolved the session may (an import rule, which no alias or
		// function value can dodge).
		Name:      "only auth/authhttp writes the principal (principalctx/principalwrite)",
		From:      []string{modulePath + "/..."},
		Forbidden: []string{modulePath + "/internal/principalctx/principalwrite"},
		Except:    []string{modulePath + "/auth/authhttp"},
		Direct:    true,
	},
	{
		Name:      "only auth, auth/authhttp and broadcast read the principal's context key",
		From:      []string{modulePath + "/..."},
		Forbidden: []string{modulePath + "/internal/principalctx"},
		Except:    []string{modulePath + "/auth", modulePath + "/auth/authhttp", modulePath + "/broadcast", modulePath + "/internal/principalctx/..."},
		Direct:    true,
	},
	{
		Name:      "only the programs under cmd/ and the example applications import the root package; framework packages import command",
		From:      []string{modulePath + "/..."},
		Forbidden: []string{modulePath},
		Except:    []string{modulePath, modulePath + "/cmd/...", modulePath + "/examples/..."},
		Direct:    true,
	},
	{
		Name:      "command is a leaf: the standard library and config only",
		From:      []string{modulePath + "/command"},
		Forbidden: []string{modulePath + "/..."},
		Allowed:   []string{modulePath + "/config"},
		Direct:    true,
	},
	{
		Name:      "config does not depend on i18n",
		From:      []string{modulePath + "/config"},
		Forbidden: []string{modulePath + "/i18n"},
	},
	{
		Name:      "broadcast does not depend on auth",
		From:      []string{modulePath + "/broadcast"},
		Forbidden: []string{modulePath + "/auth", modulePath + "/auth/..."},
	},
	{
		// ADR 0053: the core has no Argon2; the identity stores register
		// golang.org/x/crypto/argon2 with auth.RegisterArgon2id.
		Name:      "only the identity stores import golang.org/x/crypto/argon2",
		From:      []string{modulePath + "/..."},
		Forbidden: []string{"golang.org/x/crypto/argon2"},
		Except:    []string{modulePath + "/auth/postgres", modulePath + "/auth/mysql", modulePath + "/auth/sqlite"},
		Direct:    true,
	},
	{
		// ADR 0052 A-5 as amended by ADR 0053: the stores delegate their
		// Commands() to queuecmd, which stays independent of every store.
		Name:      "queuecmd imports no queue store",
		From:      []string{modulePath + "/queue/queuecmd"},
		Forbidden: []string{modulePath + "/queue/postgres", modulePath + "/queue/mysql", modulePath + "/queue/sqlite"},
	},
}

// matchesPath reports whether path is one of patterns, where "x/..." matches
// x and everything under it.
func matchesPath(path string, patterns []string) bool {
	for _, pattern := range patterns {
		if base, ok := strings.CutSuffix(pattern, "/..."); ok {
			if path == base || strings.HasPrefix(path, base+"/") {
				return true
			}
		} else if path == pattern {
			return true
		}
	}
	return false
}

// importRuleViolations checks rules against listing, the output of
// go list -f '{{.ImportPath}}{{range .Imports}} {{.}}{{end}}' ./...
func importRuleViolations(listing string, rules []importRule) []string {
	imports := map[string][]string{}
	var packages []string
	for line := range strings.Lines(listing) {
		if fields := strings.Fields(line); len(fields) > 0 {
			if _, listed := imports[fields[0]]; !listed {
				packages = append(packages, fields[0])
			}
			imports[fields[0]] = append(imports[fields[0]], fields[1:]...)
		}
	}
	slices.Sort(packages)
	var problems []string
	for _, rule := range rules {
		for _, from := range packages {
			if !matchesPath(from, rule.From) || matchesPath(from, rule.Except) {
				continue
			}
			// Breadth-first through the module's packages, remembering how
			// each was reached for the message.
			via := map[string]string{from: ""}
			queue := []string{from}
			for len(queue) > 0 {
				current := queue[0]
				queue = queue[1:]
				for _, dep := range imports[current] {
					if _, seen := via[dep]; seen {
						continue
					}
					via[dep] = current
					if matchesPath(dep, rule.Forbidden) && !matchesPath(dep, rule.Allowed) {
						chain := []string{dep}
						for at := current; at != ""; at = via[at] {
							chain = append([]string{at}, chain...)
						}
						problems = append(problems, rule.Name+": "+strings.Join(chain, " -> "))
						continue
					}
					if !rule.Direct && !matchesPath(dep, rule.Stop) {
						queue = append(queue, dep)
					}
				}
			}
		}
	}
	return problems
}

func TestImportRules(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	t.Parallel()
	output, err := exec.Command("go", "list", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, output)
	}
	for _, problem := range importRuleViolations(string(output), importRules) {
		t.Error(problem)
	}
}

func TestImportRuleViolationsFollowsImportsAndHonoursExceptions(t *testing.T) {
	t.Parallel()
	clean := strings.Join([]string{
		modulePath + " " + modulePath + "/command net/http",
		modulePath + "/cmd/forge " + modulePath,
		modulePath + "/examples/basic-api " + modulePath,
		modulePath + "/command " + modulePath + "/config io",
		modulePath + "/config " + modulePath + "/internal/langtag",
		modulePath + "/auth " + modulePath + "/fault " + modulePath + "/tenancy",
		modulePath + "/fault net/http",
		modulePath + "/auth/authhttp " + modulePath + "/auth " + modulePath + "/web net/http",
		modulePath + "/broadcast " + modulePath + "/internal/principalctx " + modulePath + "/web",
		modulePath + "/queue/postgres " + modulePath + "/queue/queuecmd " + modulePath + "/command",
		modulePath + "/queue/queuecmd " + modulePath + "/command " + modulePath + "/queue",
		"",
	}, "\n")
	if problems := importRuleViolations(clean, importRules); len(problems) != 0 {
		t.Fatalf("the allowed graph was refused: %v", problems)
	}
	for name, extra := range map[string]string{
		"auth imports web":               modulePath + "/auth " + modulePath + "/web",
		"auth reaches i18n":              modulePath + "/auth " + modulePath + "/tenancy2\n" + modulePath + "/tenancy2 " + modulePath + "/i18n",
		"auth imports net/http":          modulePath + "/auth net/http",
		"auth imports net/http/cookie":   modulePath + "/auth net/http/cookiejar",
		"auth reaches net/http":          modulePath + "/auth " + modulePath + "/auth/internal/x\n" + modulePath + "/auth/internal/x net/http",
		"views reads the principal key":  modulePath + "/views " + modulePath + "/internal/principalctx",
		"broadcast writes the principal": modulePath + "/broadcast " + modulePath + "/internal/principalctx/principalwrite",
		"a framework package uses root":  modulePath + "/migrate " + modulePath,
		"command imports uuid":           modulePath + "/command " + modulePath + "/uuid",
		"config reaches i18n":            modulePath + "/config " + modulePath + "/x\n" + modulePath + "/x " + modulePath + "/i18n",
		"broadcast reaches auth":         modulePath + "/broadcast " + modulePath + "/views2\n" + modulePath + "/views2 " + modulePath + "/auth/authhttp",
		"queuecmd uses a queue store":    modulePath + "/queue/queuecmd " + modulePath + "/queue/sqlite",
		"queuecmd reaches a queue store": modulePath + "/queue/queuecmd " + modulePath + "/x\n" + modulePath + "/x " + modulePath + "/queue/mysql",
	} {
		if problems := importRuleViolations(clean+extra+"\n", importRules); len(problems) != 1 {
			t.Errorf("%s: problems %v, want exactly one", name, problems)
		}
	}
}
