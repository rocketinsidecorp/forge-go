package cli

import (
	"fmt"
	"io"
	"strings"
	"time"
)

const modelUsage = "usage: forge generate model NAME field:type... [--org-scoped | --global] [--plural PLURAL] [--dry-run]"

// modelDirectories are the directories a model writes into.
var modelDirectories = []string{"db/migrations", "app/domain", "app/application", "app/adapters/persistence"}

// parseModelArgs reads the model command line: the resource's, without the
// flags that only make sense with an HTTP layer.
func parseModelArgs(args []string) (resourceRequest, error) {
	request, err := parseGeneratorArgs(args, modelUsage)
	if err != nil {
		return resourceRequest{}, err
	}
	if request.Broadcast {
		return resourceRequest{}, fmt.Errorf("--broadcast is only for `forge generate scaffold`: a model has no pages to update\n%s", modelUsage)
	}
	if request.Public {
		return resourceRequest{}, fmt.Errorf("--public is only for `forge generate resource` and `forge generate scaffold`: a model has no routes\n%s", modelUsage)
	}
	request.Model = true
	return request, nil
}

// runGenerateModel is `forge generate model`: the model and persistence layers
// of `forge generate resource` (migration, domain entity, repository port and
// use cases, persistence adapter, with tests) without the HTTP layer.
func runGenerateModel(args []string, stdout io.Writer, root string, env []string, now time.Time) error {
	request, err := parseModelArgs(args)
	if err != nil {
		return err
	}
	spec, files, err := planResource(root, request, env, now)
	if err != nil {
		return err
	}
	if !request.DryRun {
		if err := applyGeneratedFiles(root, modelDirectories, files); err != nil {
			return err
		}
	}
	return reportFiles(stdout, request.DryRun, files, modelWiring(spec))
}

// modelWiring is what `forge generate model` prints: how to build the use
// cases where they are needed. A model has no routes, so there is nothing to
// guard and nothing to register.
func modelWiring(spec resourceSpec) string {
	title := exportedName(spec.Name)
	var out strings.Builder
	fmt.Fprintf(&out, "\nForge did not edit app/bootstrap/bootstrap.go. Build the %s use cases where you need them:\n\n", spec.Name)
	out.WriteString("  1. Add these imports (skip any already present):\n\n")
	for _, path := range []string{"time", spec.Module + "/app/adapters/persistence", spec.Module + "/app/application", "github.com/rocketinsidecorp/forge-go/uuid"} {
		fmt.Fprintf(&out, "\t%q\n", path)
	}
	out.WriteString("\n  2. Next to the database you opened in Configure:\n\n")
	fmt.Fprintf(&out, "\t%[1]sService, err := application.New%[2]sService(persistence.New%[2]sRepository(database), time.Now, uuid.New)\n\tif err != nil {\n\t\treturn err\n\t}\n", lowerCamel(spec.Name), title)
	out.WriteString("\n  The service offers Create, Get, List, Update and Delete; pass it to the code that needs it (Go refuses a variable nothing uses).\n")
	if spec.TenantScoped {
		fmt.Fprintf(&out, "\n  %s is tenant-scoped: every call needs a tenant in its context (tenancy.WithContext), or it fails with tenant_required.\n", title)
	}
	if spec.Identity {
		fmt.Fprintf(&out, "\n  The migration registers the permissions %[1]s:read and %[1]s:write for the routes you add later.\n", spec.Plural)
	}
	out.WriteString("\n  Then: forge migrate && forge test\n")
	return out.String()
}

// reportFiles lists the files (planned, for --dry-run) and prints text after
// them.
func reportFiles(stdout io.Writer, dryRun bool, files []generatedFile, after string) error {
	verb := "create"
	if dryRun {
		verb = "would create"
	}
	for _, file := range files {
		verb := verb
		if file.identical {
			verb = "identical"
		}
		if _, err := fmt.Fprintf(stdout, "  %s  %s\n", verb, file.path); err != nil {
			return err
		}
	}
	_, err := io.WriteString(stdout, after)
	return err
}
