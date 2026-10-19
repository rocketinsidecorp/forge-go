package cli

import (
	"errors"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const scaffoldUsage = "usage: forge generate scaffold NAME field:type... (--tenant-scoped | --global) [--plural PLURAL] [--dry-run]"

// scaffoldReservedPlurals are the first path segments the application or the
// view tree already owns: /assets and /health are routes Forge registers,
// /auth and /v1 belong to the identity and JSON routes, /session and
// /passwords (with app/views/sessions and app/views/passwords) to the
// sign-in pages, and app/views/layouts and app/views/shared hold the layout
// and its partials, not pages.
var scaffoldReservedPlurals = []string{"assets", "auth", "health", "v1", "layouts", "shared", "session", "sessions", "passwords"}

// runGenerateScaffold is `forge generate scaffold`: everything `forge generate
// resource` generates, plus server-rendered HTML pages (index, show, new,
// edit, create, update, destroy) with their templates and tests.
func runGenerateScaffold(args []string, stdout io.Writer, root string, env []string, now time.Time) error {
	request, err := parseGeneratorArgs(args, scaffoldUsage)
	if err != nil {
		return err
	}
	request.Scaffold = true
	if slices.Contains(scaffoldReservedPlurals, request.Plural) {
		return fmt.Errorf("plural %q is a path the application or its view tree already uses (%s); pass another --plural", request.Plural, strings.Join(scaffoldReservedPlurals, ", "))
	}
	spec, files, err := planResourceWith(root, request, env, now, planScaffoldFiles)
	if err != nil {
		return err
	}
	if !request.DryRun {
		if err := applyGeneratedFiles(root, directoriesOf(files), files); err != nil {
			return err
		}
	}
	return reportGenerated(stdout, spec, files)
}

// planScaffoldFiles renders the HTML adapter, its tests and the templates, and
// the views base the application lacks.
func planScaffoldFiles(projectRoot *os.Root, spec *resourceSpec) ([]generatedFile, error) {
	spec.BrowserPipeline = hasBrowserPipeline(projectRoot)
	spec.ViewRenderer = hasViewRenderer(projectRoot)
	view := newScaffoldView(*spec)
	var files []generatedFile
	for _, source := range []struct{ path, template string }{
		{filepath.Join("app", "adapters", "htmlui", spec.Name+".go"), "htmlui.go.tmpl"},
		{filepath.Join("app", "adapters", "htmlui", spec.Name+"_test.go"), "htmlui_test.go.tmpl"},
	} {
		content, err := renderScaffoldTemplate(source.template, view)
		if err != nil {
			return nil, err
		}
		formatted, err := format.Source([]byte(content))
		if err != nil {
			return nil, fmt.Errorf("format generated %s: %w", source.path, err)
		}
		files = append(files, generatedFile{path: source.path, content: string(formatted)})
	}
	for _, page := range scaffoldPages {
		content, err := renderScaffoldTemplate(page.template, view)
		if err != nil {
			return nil, err
		}
		files = append(files, generatedFile{path: filepath.Join("app", "views", spec.Plural, page.name+".gohtml"), content: content})
	}
	base, err := viewBaseFiles(projectRoot, spec.Module)
	if err != nil {
		return nil, errors.Join(errors.New("the pages need the application's views package"), err)
	}
	return append(files, base...), nil
}

// hasBrowserPipeline reports whether bootstrap.go appears to install CSRF
// protection and the method override, as `forge new` does.
func hasBrowserPipeline(projectRoot *os.Root) bool {
	source, err := projectRoot.ReadFile(filepath.Join("app", "bootstrap", "bootstrap.go"))
	if err != nil {
		return false
	}
	text := string(source)
	return strings.Contains(text, "csrf.New(") && strings.Contains(text, "web.MethodOverride") && strings.Contains(text, "web.Flash")
}

// hasViewRenderer reports whether bootstrap.go already builds the shared view
// renderer, so the printed wiring reuses it instead of declaring it twice.
func hasViewRenderer(projectRoot *os.Root) bool {
	source, err := projectRoot.ReadFile(filepath.Join("app", "bootstrap", "bootstrap.go"))
	return err == nil && strings.Contains(string(source), "renderer, err := views.New()")
}
