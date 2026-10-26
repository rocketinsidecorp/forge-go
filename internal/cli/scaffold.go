package cli

import (
	"errors"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/i18n"
)

const scaffoldUsage = "usage: forge generate scaffold NAME field:type... (--tenant-scoped | --global) [--plural PLURAL] [--broadcast] [--dry-run]"

// scaffoldReservedPlurals are the first path segments the application or the
// view tree already owns: /assets and /health are routes Forge registers,
// /auth and /v1 belong to the identity and JSON routes, /session and
// /passwords (with app/views/sessions and app/views/passwords) to the
// sign-in pages, and app/views/layouts and app/views/shared hold the layout
// and its partials, not pages.
var scaffoldReservedPlurals = []string{"assets", "auth", "health", "v1", "layouts", "shared", "session", "sessions", "passwords"}

// scaffoldFormattingScopes are plural names whose translation scope would
// merge into the formatting data (date.formats, number.format, time.am) that
// `l` and `number` read and `forge i18n check` skips.
var scaffoldFormattingScopes = []string{"date", "time", "number"}

// runGenerateScaffold is `forge generate scaffold`: everything `forge generate
// resource` generates, plus server-rendered HTML pages (index, show, new,
// edit, create, update, destroy) with their templates and tests.
func runGenerateScaffold(args []string, stdout io.Writer, root string, env []string, now time.Time) error {
	request, err := parseGeneratorArgs(args, scaffoldUsage)
	if err != nil {
		return err
	}
	request.Scaffold = true
	if apiOnlyAt(root) {
		return errors.New(apiOnlyScaffoldRefusal)
	}
	if slices.Contains(scaffoldReservedPlurals, request.Plural) {
		return fmt.Errorf("plural %q is a path the application or its view tree already uses (%s); pass another --plural", request.Plural, strings.Join(scaffoldReservedPlurals, ", "))
	}
	if slices.Contains(scaffoldFormattingScopes, request.Plural) {
		return fmt.Errorf("plural %q is a formatting scope of the locale files (%s), which `l` and `number` read and `forge i18n check` skips; pass another --plural", request.Plural, strings.Join(scaffoldFormattingScopes, ", "))
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
	spec.I18nMiddleware = hasI18nMiddleware(projectRoot)
	spec.ViewRenderer = hasViewRenderer(projectRoot)
	view := newScaffoldView(*spec)
	var files []generatedFile
	sources := []struct{ path, template string }{
		{filepath.Join("app", "adapters", "htmlui", spec.Name+".go"), "htmlui.go.tmpl"},
		{filepath.Join("app", "adapters", "htmlui", spec.Name+"_test.go"), "htmlui_test.go.tmpl"},
	}
	if spec.Broadcast && !spec.Destroy {
		// The live-update transport is shared by every broadcasting scaffold:
		// the first one adds it, later ones reuse it.
		streams := filepath.Join("app", "adapters", "htmlstreams", "streams.go")
		if _, err := projectRoot.Lstat(streams); errors.Is(err, os.ErrNotExist) {
			spec.AddsStreams = true
			sources = append(sources,
				struct{ path, template string }{streams, "htmlstreams.go.tmpl"},
				struct{ path, template string }{filepath.Join("app", "adapters", "htmlstreams", "streams_test.go"), "htmlstreams_test.go.tmpl"},
			)
		} else if err != nil {
			return nil, err
		}
	}
	for _, source := range sources {
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
	// The text is English; an application whose default locale is another one
	// gets it under that locale too, so production never falls back to
	// humanized keys, and `forge i18n check` finds nothing missing.
	spec.DefaultLocale = appDefaultLocale(projectRoot)
	locales := []string{i18n.DefaultLocale}
	if spec.DefaultLocale != i18n.DefaultLocale {
		locales = append(locales, spec.DefaultLocale)
	}
	for _, locale := range locales {
		translations, err := view.Translations(locale)
		if err != nil {
			return nil, err
		}
		files = append(files, generatedFile{
			path:        filepath.Join(localesDir, spec.Plural+"."+locale+".json"),
			content:     translations,
			translation: locale != i18n.DefaultLocale,
		})
	}
	for _, page := range scaffoldPages {
		if page.broadcastOnly && !spec.Broadcast {
			continue
		}
		content, err := renderScaffoldTemplate(page.template, view)
		if err != nil {
			return nil, err
		}
		files = append(files, generatedFile{path: filepath.Join("app", "views", spec.Plural, page.name+".gohtml"), content: content})
	}
	if spec.Destroy {
		// The views base and the live-update transport are shared with the
		// rest of the application; destroying one scaffold never touches them.
		return files, nil
	}
	if spec.Broadcast {
		if err := checkLiveUpdateBase(projectRoot); err != nil {
			return nil, err
		}
	}
	base, err := viewBaseFiles(projectRoot, spec.Module)
	if err != nil {
		return nil, errors.Join(errors.New("the pages need the application's views package"), err)
	}
	spec.AddsViewBase = len(base) > 0
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

// defaultLocalePattern matches the DefaultLocale constant of the generated
// config/locales/locales.go.
var defaultLocalePattern = regexp.MustCompile(`(?m)^const DefaultLocale = "([^"]+)"`)

// appDefaultLocale is the application's default locale: the DefaultLocale
// constant of config/locales/locales.go (`forge new --locale`), else "en" for
// applications that predate i18n or edited the file beyond recognition.
// FORGE_I18N_DEFAULT_LOCALE is a deployment override and does not apply.
func appDefaultLocale(projectRoot *os.Root) string {
	source, err := projectRoot.ReadFile(filepath.Join(localesDir, "locales.go"))
	if err != nil {
		return i18n.DefaultLocale
	}
	match := defaultLocalePattern.FindSubmatch(source)
	if match == nil {
		return i18n.DefaultLocale
	}
	locale, err := i18n.CanonicalLocale(string(match[1]))
	if err != nil {
		return i18n.DefaultLocale
	}
	return locale
}

// hasI18nMiddleware reports whether bootstrap.go appears to build the
// translator and install its middleware, as `forge new` does.
func hasI18nMiddleware(projectRoot *os.Root) bool {
	source, err := projectRoot.ReadFile(filepath.Join("app", "bootstrap", "bootstrap.go"))
	if err != nil {
		return false
	}
	text := string(source)
	return strings.Contains(text, "locales.New(") && strings.Contains(text, ".Middleware(i18n.NegotiationOptions")
}

// hasViewRenderer reports whether bootstrap.go already builds the shared view
// renderer, so the printed wiring reuses it instead of declaring it twice.
func hasViewRenderer(projectRoot *os.Root) bool {
	source, err := projectRoot.ReadFile(filepath.Join("app", "bootstrap", "bootstrap.go"))
	return err == nil && strings.Contains(string(source), "renderer, err := views.New(")
}

// checkLiveUpdateBase refuses --broadcast in an application whose views
// package predates live updates: its pages could not sign stream names, and
// the renderer would stop at startup.
func checkLiveUpdateBase(projectRoot *os.Root) error {
	source, err := projectRoot.ReadFile(filepath.Join("app", "views", "views.go"))
	if errors.Is(err, os.ErrNotExist) {
		return nil // the views base is generated with it
	}
	if err != nil {
		return err
	}
	if !strings.Contains(string(source), "func WithStreams(") || !strings.Contains(string(source), "Assets:") {
		return errors.New("--broadcast needs the asset bundle and the WithStreams option that `forge new` now generates in app/views/views.go (and app/assets, app/javascript), but this application's views package predates them: see \"Live updates\" in docs/VIEWS.md to add them, or generate a new application and compare")
	}
	return nil
}
