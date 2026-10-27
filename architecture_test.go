package forge_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const modulePath = "github.com/rocketinsidecorp/forge-go"

// corePackages may import only the standard library and each other (ADR 0006,
// ADR 0007). Applications that never touch the database therefore never link
// a third-party module through Forge.
var corePackages = []string{
	modulePath,
	modulePath + "/assets",
	modulePath + "/auth",
	modulePath + "/auth/authhttp",
	modulePath + "/auth/recoverymail",
	modulePath + "/auth/recoveryqueue",
	modulePath + "/cache",
	modulePath + "/command",
	modulePath + "/cache/cachetest",
	modulePath + "/broadcast",
	modulePath + "/agent",
	modulePath + "/config",
	modulePath + "/csrf",
	modulePath + "/credentials",
	modulePath + "/dbtest",
	modulePath + "/encryption",
	modulePath + "/events",
	modulePath + "/fault",
	modulePath + "/health",
	modulePath + "/hotwire",
	modulePath + "/i18n",
	modulePath + "/internal/langtag",
	modulePath + "/internal/principalctx",
	modulePath + "/mail",
	modulePath + "/mail/mailtest",
	modulePath + "/jobs",
	modulePath + "/jobs/ticktest",
	modulePath + "/migrate",
	modulePath + "/migrate/migratetest",
	modulePath + "/openapi",
	modulePath + "/httpserver",
	modulePath + "/pagination",
	modulePath + "/queue",
	modulePath + "/queue/internal/jobscmd",
	modulePath + "/queue/queuecmd",
	modulePath + "/queue/storetest",
	modulePath + "/ratelimit",
	modulePath + "/ratelimit/limitertest",
	modulePath + "/router",
	modulePath + "/sqldb",
	modulePath + "/storage",
	modulePath + "/storage/disk",
	modulePath + "/storage/storagetest",
	modulePath + "/telemetry",
	modulePath + "/telemetry/telemetrytest",
	modulePath + "/tenancy",
	modulePath + "/turbo",
	modulePath + "/uuid",
	modulePath + "/views",
	modulePath + "/web",
}

// coreThirdPartyImports admits packages outside the standard library and the
// core, each by its own ADR as ADR 0006 requires. Packages, not modules: the
// rest of golang.org/x/crypto stays out. The value lists the only core
// packages that may import the package directly; with none, the package may
// only be reached through another admitted one.
var coreThirdPartyImports = map[string][]string{
	// ADR 0048: auth's Argon2id password hashing (golang.org/x/crypto, Go
	// team, BSD-3-Clause) and what argon2 imports.
	"golang.org/x/crypto/argon2":  {modulePath + "/auth"},
	"golang.org/x/crypto/blake2b": nil,
	"golang.org/x/sys/cpu":        nil,
}

// coreImportViolations checks the output of
// go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{range .Imports}} {{.}}{{end}}{{end}}' pkg.
func coreImportViolations(pkg, listing string) []string {
	var problems []string
	for line := range strings.Lines(listing) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		dep, imports := fields[0], fields[1:]
		if !slices.Contains(corePackages, dep) {
			if _, admitted := coreThirdPartyImports[dep]; !admitted {
				problems = append(problems, "core package "+pkg+" depends on "+dep+"; core may import only the standard library and core packages")
			}
			continue
		}
		for _, imported := range imports {
			thirdParty := !slices.Contains(corePackages, imported) && strings.Contains(strings.SplitN(imported, "/", 2)[0], ".")
			if thirdParty && !slices.Contains(coreThirdPartyImports[imported], dep) {
				problems = append(problems, "core package "+dep+" imports "+imported+"; only "+strings.Join(coreThirdPartyImports[imported], ", ")+" may (ADR 0048)")
			}
		}
	}
	return problems
}

func TestCorePackagesDependOnlyOnStandardLibraryAndCore(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	t.Parallel()
	for _, pkg := range corePackages {
		output, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{range .Imports}} {{.}}{{end}}{{end}}", pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", pkg, err, output)
		}
		for _, problem := range coreImportViolations(pkg, string(output)) {
			t.Error(problem)
		}
	}
}

func TestCoreThirdPartyImportsAreScopedToTheirImporter(t *testing.T) {
	t.Parallel()
	auth, views := modulePath+"/auth", modulePath+"/views"
	allowed := auth + " golang.org/x/crypto/argon2 errors\ngolang.org/x/crypto/argon2 golang.org/x/crypto/blake2b golang.org/x/sys/cpu\ngolang.org/x/crypto/blake2b golang.org/x/sys/cpu\ngolang.org/x/sys/cpu\n" + views + " " + auth + "\n"
	if problems := coreImportViolations(views, allowed); len(problems) != 0 {
		t.Fatalf("auth importing argon2, reached from views, refused: %v", problems)
	}
	for name, listing := range map[string]string{
		"another core package imports argon2": views + " golang.org/x/crypto/argon2\ngolang.org/x/crypto/argon2\n",
		"auth imports blake2b directly":       auth + " golang.org/x/crypto/blake2b\ngolang.org/x/crypto/blake2b\n",
		"auth imports other x/crypto":         auth + " golang.org/x/crypto/sha3\ngolang.org/x/crypto/sha3\n",
	} {
		if problems := coreImportViolations(auth, listing); len(problems) == 0 {
			t.Errorf("%s: accepted", name)
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
		// and so links net/http; auth itself must not use it.
		Name:      "auth does not use net/http",
		From:      []string{modulePath + "/auth"},
		Forbidden: []string{"net/http", "net/http/..."},
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
		Name:      "the queue stores do not depend on queuecmd",
		From:      []string{modulePath + "/queue/postgres", modulePath + "/queue/mysql", modulePath + "/queue/sqlite"},
		Forbidden: []string{modulePath + "/queue/queuecmd"},
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
					if !rule.Direct {
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
		modulePath + "/queue/postgres " + modulePath + "/queue/internal/jobscmd " + modulePath + "/command",
		modulePath + "/queue/queuecmd " + modulePath + "/queue/internal/jobscmd",
		"",
	}, "\n")
	if problems := importRuleViolations(clean, importRules); len(problems) != 0 {
		t.Fatalf("the allowed graph was refused: %v", problems)
	}
	for name, extra := range map[string]string{
		"auth imports web":              modulePath + "/auth " + modulePath + "/web",
		"auth reaches i18n":             modulePath + "/auth " + modulePath + "/tenancy2\n" + modulePath + "/tenancy2 " + modulePath + "/i18n",
		"auth imports net/http":         modulePath + "/auth net/http",
		"auth imports net/http/cookie":  modulePath + "/auth net/http/cookiejar",
		"a framework package uses root": modulePath + "/migrate " + modulePath,
		"command imports uuid":          modulePath + "/command " + modulePath + "/uuid",
		"config reaches i18n":           modulePath + "/config " + modulePath + "/x\n" + modulePath + "/x " + modulePath + "/i18n",
		"broadcast reaches auth":        modulePath + "/broadcast " + modulePath + "/views2\n" + modulePath + "/views2 " + modulePath + "/auth/authhttp",
		"a queue store uses queuecmd":   modulePath + "/queue/sqlite " + modulePath + "/queue/queuecmd",
	} {
		if problems := importRuleViolations(clean+extra+"\n", importRules); len(problems) != 1 {
			t.Errorf("%s: problems %v, want exactly one", name, problems)
		}
	}
}
