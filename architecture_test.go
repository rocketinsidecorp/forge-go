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
