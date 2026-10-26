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

// coreThirdPartyPackages are the only packages outside the standard library
// and the core that the core may link, each admitted by its own ADR as ADR
// 0006 requires. Packages, not modules: the rest of golang.org/x/crypto stays
// out.
var coreThirdPartyPackages = []string{
	// ADR 0048: auth's Argon2id password hashing (golang.org/x/crypto,
	// Go team, BSD-3-Clause) and what it imports.
	"golang.org/x/crypto/argon2",
	"golang.org/x/crypto/blake2b",
	"golang.org/x/sys/cpu",
}

func TestCorePackagesDependOnlyOnStandardLibraryAndCore(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	t.Parallel()
	for _, pkg := range corePackages {
		output, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", pkg, err, output)
		}
		for _, dep := range strings.Fields(string(output)) {
			if !slices.Contains(corePackages, dep) && !slices.Contains(coreThirdPartyPackages, dep) {
				t.Errorf("core package %s depends on %s; core may import only the standard library and core packages", pkg, dep)
			}
		}
	}
}
