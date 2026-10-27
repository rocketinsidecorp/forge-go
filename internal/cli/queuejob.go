package cli

import (
	"embed"
	"errors"
	"fmt"
	"go/format"
	"io"
	"path/filepath"
	"strings"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

const queueJobUsage = "usage: forge generate job NAME [field:type...] --queue [--dry-run]"

const maxQueueJobFields = 20

//go:embed jobtemplates/*.tmpl
var jobTemplates embed.FS

// queueJobRequest is a syntactically valid `generate job --queue` command line.
type queueJobRequest struct {
	Name    string
	Fields  []resourceField
	DryRun  bool
	Destroy bool
}

// parseQueueJobArgs reads `NAME [field:type...] --queue [--dry-run]`. The
// payload fields use the resource field types (string, text, int, bool, time,
// uuid; a trailing ? makes one optional). It touches no files.
func parseQueueJobArgs(args []string) (queueJobRequest, error) {
	flags, positional, err := splitFlags(args, queueJobUsage, "--queue", "--dry-run")
	if err != nil {
		return queueJobRequest{}, err
	}
	if !flags["--queue"] {
		return queueJobRequest{}, usageError{queueJobUsage}
	}
	request := queueJobRequest{DryRun: flags["--dry-run"]}
	if len(positional) == 0 {
		return queueJobRequest{}, usageError{queueJobUsage}
	}
	request.Name = positional[0]
	if err := validateGeneratorWord("job name", request.Name); err != nil {
		return queueJobRequest{}, err
	}
	if len(positional)-1 > maxQueueJobFields {
		return queueJobRequest{}, fmt.Errorf("a job payload has at most %d fields", maxQueueJobFields)
	}
	seen := make(map[string]bool)
	for _, declaration := range positional[1:] {
		field, err := parseField(declaration, false)
		if err != nil {
			return queueJobRequest{}, err
		}
		exported := exportedName(field.Name)
		if seen[exported] {
			return queueJobRequest{}, fmt.Errorf("field %q is declared twice (or exports to the same Go field as another)", field.Name)
		}
		seen[exported] = true
		request.Fields = append(request.Fields, field)
	}
	return request, nil
}

// runGenerateQueueJob is `forge generate job NAME --queue`: a typed handler
// for the persistent job queue (package queue), the function that enqueues it,
// and tests that need no database.
func runGenerateQueueJob(args []string, stdout io.Writer, root string, env []string) error {
	request, err := parseQueueJobArgs(args)
	if err != nil {
		return err
	}
	module, files, err := planQueueJob(root, request)
	if err != nil {
		return err
	}
	// The seam is planned first, so a refused seam writes nothing.
	wiring, wired, err := planQueueJobSeam(root, request, env, false)
	if err != nil {
		return err
	}
	if !request.DryRun {
		if err := applyGeneratedFiles(root, directoriesOf(files), files); err != nil {
			return err
		}
		if wired && wiring.verb == "wire" {
			if err := writeRoutes(root, wiring.content); err != nil {
				return err
			}
		}
	}
	if !wired {
		return reportFiles(stdout, request.DryRun, files, queueJobWiring(request, module))
	}
	return reportFiles(stdout, request.DryRun, files, reportSeam(wiring, request.DryRun, "forge test"))
}

// planQueueJobSeam plans the routes seam with the job's registration added
// (or, for `forge destroy`, removed): jobs.RegisterNAME(registry), on the
// registry QueueConfigure runs a worker for. wired is false in an
// application that does not pass the registry to the seam (one created before
// forge new wired the queue): the generator then prints the registration.
func planQueueJobSeam(root string, request queueJobRequest, env []string, remove bool) (wiring seamWiring, wired bool, err error) {
	projectRoot, module, err := openApplication(root)
	if err != nil {
		return seamWiring{}, false, err
	}
	defer func() { _ = projectRoot.Close() }()
	adapter, err := databaseAdapter(root, env)
	if err != nil {
		return seamWiring{}, false, errors.New("environment files could not be loaded: check .env.development and .env.local syntax")
	}
	identity := hasIdentityMigrations(projectRoot, adapter) && !apiOnlyApplication(projectRoot)
	seam := openRoutesSeam(projectRoot, module, adapter, identity)
	if !seam.app.Queue {
		return seamWiring{}, false, nil
	}
	title := exportedName(request.Name)
	body := fmt.Sprintf("\tif err := jobs.Register%s(registry); err != nil {\n\t\treturn err\n\t}\n", title)
	content, verb, err := planRoutes(projectRoot, seam, routesChange{
		entry:  scaffold.RouteBlock{Kind: "job", Name: request.Name, Body: body},
		remove: remove,
	})
	notes := []string{
		fmt.Sprintf("Enqueue it, atomically with your data when you can:\n\n\t_, err := jobs.Enqueue%s(ctx, store.In(tx), registry, jobs.%sPayload{%s})", title, title, queueJobPayloadExample(request)),
		"Replace Perform" + title + ", which reports ErrNotImplemented (a permanent failure, so the job is dead and visible in `jobs dead`) until you do. The worker (QueueConfigure in app/bootstrap/bootstrap.go) starts with the first registered handler, while the application serves.",
	}
	return seamWiring{content: content, verb: verb, notes: notes}, true, err
}

type queueJobView struct {
	Name    string
	Title   string
	Camel   string
	Fields  []fieldView
	HasTime bool
	HasUUID bool
}

func newQueueJobView(request queueJobRequest) queueJobView {
	view := queueJobView{Name: request.Name, Title: exportedName(request.Name), Camel: lowerCamel(request.Name)}
	for _, field := range request.Fields {
		view.Fields = append(view.Fields, fieldView{resourceField: field, Title: exportedName(field.Name), Camel: view.Camel})
		view.HasTime = view.HasTime || field.Type == "time"
		view.HasUUID = view.HasUUID || field.Type == "uuid"
	}
	return view
}

// planQueueJob renders the job and its test. Nothing is written.
func planQueueJob(root string, request queueJobRequest) (module string, files []generatedFile, err error) {
	projectRoot, module, err := openApplication(root)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = projectRoot.Close() }()
	view := newQueueJobView(request)
	for _, source := range []struct{ suffix, template string }{
		{".go", "queue_job.go.tmpl"},
		{"_test.go", "queue_job_test.go.tmpl"},
	} {
		content, err := renderEmbedded(jobTemplates, "jobtemplates/"+source.template, view)
		if err != nil {
			return "", nil, err
		}
		path := filepath.Join("app", "jobs", request.Name+source.suffix)
		formatted, err := format.Source([]byte(content))
		if err != nil {
			return "", nil, fmt.Errorf("format generated %s: %w", path, err)
		}
		files = append(files, generatedFile{path: path, content: string(formatted)})
	}
	if err := checkPlannedFiles(projectRoot, files, request.Destroy); err != nil {
		return "", nil, err
	}
	return module, files, nil
}

// queueJobWiring is what `forge generate job --queue` prints: how to register
// the handler and enqueue the job. The generator never edits bootstrap.go.
func queueJobWiring(request queueJobRequest, module string) string {
	title := exportedName(request.Name)
	var out strings.Builder
	out.WriteString("\nForge did not edit app/bootstrap/bootstrap.go. Register the handler where you build the queue registry:\n\n")
	out.WriteString("  1. Add this import (skip it if already present):\n\n")
	fmt.Fprintf(&out, "\t%q\n", module+"/app/jobs")
	out.WriteString("\n  2. Next to the other handlers, before the worker starts:\n\n")
	fmt.Fprintf(&out, "\tif err := jobs.Register%s(registry); err != nil {\n\t\treturn err\n\t}\n", title)
	out.WriteString("\n  3. Enqueue it, atomically with your data when you can:\n\n")
	fmt.Fprintf(&out, "\t_, err := jobs.Enqueue%s(ctx, store.In(tx), registry, jobs.%sPayload{%s})\n", title, title, queueJobPayloadExample(request))
	out.WriteString("\n  Replace Perform" + title + ", which reports ErrNotImplemented (a permanent failure, so the job is dead and visible in `jobs dead`) until you do.\n")
	out.WriteString("  An application without the queue needs its tables, command and worker first: docs/JOBS.md, sections 1 and 4.\n")
	out.WriteString("\n  Then: forge test\n")
	return out.String()
}

func queueJobPayloadExample(request queueJobRequest) string {
	if len(request.Fields) == 0 {
		return ""
	}
	return "..."
}
