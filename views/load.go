package views

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rocketinsidecorp/forge-go/assets"
	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/broadcast"
	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/i18n"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/turbo"
	"github.com/rocketinsidecorp/forge-go/web"
)

// DefaultLayout is the layout pages use unless Options.Layout names another.
const DefaultLayout = "application"

// DefaultContentSecurityPolicy is sent with every page a loaded Renderer
// renders: same-origin scripts, styles, images, fonts and fetches; forms
// post only to this origin; no inline scripts, styles or event handlers; no
// framing.
const DefaultContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

const contentBlock = "content"

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

// Options configures Load.
type Options struct {
	// Layout is the default layout, a file in layouts/ without its extension.
	// Empty means DefaultLayout.
	Layout string
	// Funcs adds application template functions. Names may not replace the
	// built-in helpers. Functions returning template.HTML bypass escaping and
	// must never build it from unescaped input.
	Funcs template.FuncMap
	// ContentSecurityPolicy replaces the default policy entirely. The
	// import map hash and StyleHashes are then not added: include
	// ImportMap.CSPSource() yourself.
	ContentSecurityPolicy string

	// Assets is the digested asset table. It enables the assetPath,
	// stylesheetTag and javascriptImportmapTags helpers; a page that uses one
	// without it stops Load.
	Assets *assets.Set
	// ImportMap is rendered by javascriptImportmapTags. The default policy
	// allows its inline script by SHA-256 hash, never with 'unsafe-inline'.
	ImportMap *assets.ImportMap
	// StyleHashes are base64 SHA-256 hashes of inline <style> elements the
	// default policy should allow, such as hotwire.StyleHashes() for Turbo's
	// progress bar.
	StyleHashes []string

	// Streams signs the channel names turboStreamFrom renders. It enables
	// turboStreamFrom and turboStreamFromGlobal.
	Streams *broadcast.Signer
	// StreamsPath is where the event stream handler is mounted. Empty means
	// "/streams".
	StreamsPath string

	// I18n translates the t, l and number helpers when the request carries
	// no translator (i18n.Translator.Middleware puts one in the request
	// context), such as fragments rendered for a broadcast without a
	// request; they then use its default locale.
	I18n *i18n.Translator
}

// page is one renderable template set: the layouts, the partials and one
// page file. pristine is never executed, so it can always be cloned; the
// pool holds escaped clones whose request helpers read their own state.
type page struct {
	entry    string
	pristine *template.Template
	setup    *setup
	pool     sync.Pool
}

type instance struct {
	templates *template.Template
	state     *renderState
}

type renderState struct {
	writer  http.ResponseWriter
	request *http.Request
	// probing is set while Load checks a page's escaping without a request.
	probing bool
}

type source struct {
	name string
	text string
}

// Load parses an application's view tree:
//
//   - layouts/NAME.gohtml are layouts. They wrap a page with
//     {{block "content" .}}{{end}} (and may offer other blocks, such as
//     "title").
//   - Files whose name starts with "_" are partials, such as
//     shared/_flash.gohtml, included with {{template "shared/_flash" .}}.
//   - Every other *.gohtml file is a page named by its path without the
//     extension, such as "posts/index". A page that defines "content" is
//     rendered inside the layout; any other page is a complete document.
//
// Template names are the file paths without ".gohtml". Each page is parsed
// into its own copy of the layouts and partials, so the "content" and
// "title" blocks of different pages never collide. Escaping problems are
// reported here rather than at the first request: Load executes each page
// once with nil data into a discarding writer, so application functions in
// Options.Funcs must not have side effects.
//
// Pages may use these helpers:
//
//	csrfField          hidden authenticity_token input for a form
//	csrfMeta           csrf-param and csrf-token meta tags
//	csrfToken          the token, masked afresh per call, for data attributes
//	methodField "PATCH" hidden _method input (PUT, PATCH or DELETE)
//	flashes            the incoming web.FlashMessage values
//	flash "notice"     the text of one incoming flash message
//	domID "post" .ID   an element id: post_<id>, or new_post for a zero UUID
//	dict "k" v ...     a map, for passing several values to a partial
//	can "posts:write"  whether the signed-in Principal holds the permission,
//	                   to show only the forms and frames the caller may use;
//	                   display only (the routes still authorize), and true
//	                   when the request carries no Principal (no Forge
//	                   identity) or there is no request
//	signedIn           whether the request carries a signed-in Principal
//	                   (false without one, and without a request): show a
//	                   sign-out button only to people who are signed in
//
// With i18n.Translator.Middleware in front of the page's handler (or
// Options.I18n), see docs/I18N.md:
//
//	t "users.welcome" "name" .Name   the translation for the request's
//	                                 locale; keys ending in _html are trusted
//	                                 markup with escaped arguments
//	t "inbox" "count" .Unread        the plural form for a count
//	l .CreatedAt "short"             a time in time.formats.short
//	number .Total 2                  a number with the locale's separators
//	locale                           the request's locale, for <html lang>
//
// With Options.Assets, ImportMap and Streams (see docs/VIEWS.md):
//
//	assetPath "logo.png"              digested URL of an asset
//	stylesheetTag "application"       <link rel="stylesheet"> for application.css
//	javascriptImportmapTags "application"
//	                                  import map, modulepreload links and the
//	                                  module script of application.js
//	turboStreamFrom "posts"           <turbo-stream-source> for the request
//	                                  tenant's "posts" channel (needs a tenant)
//	turboStreamFromGlobal "banner"    the same for a channel shared by every tenant
//
// The csrf and flash helpers need RenderRequest and the csrf.Protection and
// web.Flash middleware; anywhere else they fail the render. A helper used
// without the Option that enables it stops Load.
func Load(templates fs.FS, opts Options) (*Renderer, error) {
	if templates == nil {
		return nil, errors.New("views: template filesystem is required")
	}
	layout := opts.Layout
	if layout == "" {
		layout = DefaultLayout
	}
	if !segmentPattern.MatchString(layout) {
		return nil, fmt.Errorf("views: invalid layout name %q", layout)
	}
	layout = "layouts/" + layout
	setup, err := newSetup(opts)
	if err != nil {
		return nil, err
	}
	funcs := helperFuncs(nil, setup)
	for name, function := range opts.Funcs {
		if _, builtIn := funcs[name]; builtIn {
			return nil, fmt.Errorf("views: template function %q is built in", name)
		}
		if function == nil {
			return nil, fmt.Errorf("views: template function %q is nil", name)
		}
		funcs[name] = function
	}
	csp := opts.ContentSecurityPolicy
	if csp == "" {
		csp = setup.policy
	}

	shared, pages, err := readTree(templates)
	if err != nil {
		return nil, err
	}
	base := template.New("").Funcs(funcs)
	for _, file := range shared {
		if _, err := base.New(file.name).Parse(file.text); err != nil {
			return nil, fmt.Errorf("views: parse %s: %w", file.name, err)
		}
	}
	renderer := &Renderer{pages: make(map[string]*page, len(pages)), fragments: map[string]*page{}, csp: csp}
	for _, file := range pages {
		parsed, err := parsePage(base, funcs, setup, file, layout)
		if err != nil {
			return nil, err
		}
		renderer.pages[file.name] = parsed
	}
	for _, file := range shared {
		if strings.HasPrefix(path.Base(file.name), "_") {
			fragment, err := parseFragment(base, setup, file.name)
			if err != nil {
				return nil, err
			}
			renderer.fragments[file.name] = fragment
		}
	}
	return renderer, nil
}

func readTree(templates fs.FS) (shared, pages []source, err error) {
	err = fs.WalkDir(templates, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name != "." && strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".gohtml") {
			return nil
		}
		templateName := strings.TrimSuffix(name, ".gohtml")
		for segment := range strings.SplitSeq(templateName, "/") {
			if !segmentPattern.MatchString(segment) {
				return fmt.Errorf("views: template path %q must use letters, digits, '_' and '-'", name)
			}
		}
		content, err := fs.ReadFile(templates, name)
		if err != nil {
			return fmt.Errorf("views: read %s: %w", name, err)
		}
		file := source{name: templateName, text: string(content)}
		if strings.HasPrefix(templateName, "layouts/") || strings.HasPrefix(path.Base(templateName), "_") {
			shared = append(shared, file)
		} else {
			pages = append(pages, file)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(shared)+len(pages) == 0 {
		return nil, nil, errors.New("views: no templates found; expected *.gohtml files")
	}
	return shared, pages, nil
}

func parsePage(base *template.Template, funcs template.FuncMap, setup *setup, file source, layout string) (*page, error) {
	alone, err := template.New(file.name).Funcs(funcs).Parse(file.text)
	if err != nil {
		return nil, fmt.Errorf("views: parse %s: %w", file.name, err)
	}
	entry := file.name
	if alone.Lookup(contentBlock) != nil {
		if base.Lookup(layout) == nil {
			return nil, fmt.Errorf("views: page %s defines %q but %s.gohtml does not exist", file.name, contentBlock, layout)
		}
		entry = layout
	}
	pristine, err := base.Clone()
	if err != nil {
		return nil, fmt.Errorf("views: clone templates for %s: %w", file.name, err)
	}
	if _, err := pristine.New(file.name).Parse(file.text); err != nil {
		return nil, fmt.Errorf("views: parse %s: %w", file.name, err)
	}
	parsed := &page{entry: entry, pristine: pristine, setup: setup}
	err = parsed.probe(file.name)
	return parsed, err
}

// parseFragment makes a partial renderable on its own (Renderer.Fragment).
func parseFragment(base *template.Template, setup *setup, name string) (*page, error) {
	pristine, err := base.Clone()
	if err != nil {
		return nil, fmt.Errorf("views: clone templates for %s: %w", name, err)
	}
	fragment := &page{entry: name, pristine: pristine, setup: setup}
	err = fragment.probe(name)
	return fragment, err
}

// probe escapes a throwaway clone now: html/template escapes lazily, and a
// template it cannot escape safely should stop the application at startup.
// Execution errors from the missing data are expected, except a helper that
// the options leave unconfigured, which is a setup mistake worth failing on.
func (p *page) probe(name string) error {
	probe, err := p.newInstance()
	if err != nil {
		return err
	}
	probe.state.probing = true
	var escapeErr *template.Error
	var setupErr *setupError
	if err := probe.templates.ExecuteTemplate(io.Discard, p.entry, nil); errors.As(err, &escapeErr) || errors.As(err, &setupErr) {
		return fmt.Errorf("views: %s: %w", name, err)
	}
	return nil
}

func (p *page) newInstance() (*instance, error) {
	clone, err := p.pristine.Clone()
	if err != nil {
		return nil, fmt.Errorf("views: clone templates: %w", err)
	}
	state := &renderState{}
	return &instance{templates: clone.Funcs(helperFuncs(state, p.setup)), state: state}, nil
}

func (p *page) acquire() (*instance, error) {
	if pooled, ok := p.pool.Get().(*instance); ok {
		return pooled, nil
	}
	return p.newInstance()
}

func (r *Renderer) renderPage(w http.ResponseWriter, request *http.Request, name string, status int, data any) error {
	page, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("views: page %q does not exist", name)
	}
	if err := validStatus(status); err != nil {
		return err
	}
	if status <= 0 {
		status = http.StatusOK
	}
	body, err := page.execute(w, request, name, data)
	if err != nil {
		return err
	}
	if request != nil {
		i18n.SetHeaders(w, request.Context())
	}
	w.Header().Set("Content-Security-Policy", r.csp)
	return writeHTML(w, status, body.Bytes())
}

func (p *page) execute(w http.ResponseWriter, request *http.Request, name string, data any) (*bytes.Buffer, error) {
	current, err := p.acquire()
	if err != nil {
		return nil, err
	}
	current.state.writer, current.state.request = w, request
	var body bytes.Buffer
	err = current.templates.ExecuteTemplate(&body, p.entry, data)
	current.state.writer, current.state.request = nil, nil
	p.pool.Put(current)
	if err != nil {
		return nil, fmt.Errorf("views: execute %q: %w", name, err)
	}
	return &body, nil
}

// Fragment renders a partial (a template whose file name starts with "_",
// such as "posts/_post") without a layout and returns the escaped markup, for
// Turbo Stream content (package turbo) or any other place that needs a piece
// of HTML. Pages are rendered with RenderRequest instead.
//
// request may be nil, which is what broadcasting from a job or another
// replica looks like; the csrf, flash and tenant-scoped stream helpers then
// fail the render. Fragments that are broadcast reach every subscriber, so
// they must not contain anything specific to one request (no forms with
// CSRF tokens, no per-user content).
func (r *Renderer) Fragment(request *http.Request, name string, data any) (template.HTML, error) {
	if r == nil || r.fragments == nil {
		return "", errors.New("views: Fragment needs a Renderer built by Load")
	}
	fragment, ok := r.fragments[name]
	if !ok {
		return "", fmt.Errorf("views: partial %q does not exist (partials are files named _NAME.gohtml)", name)
	}
	// The writer is only used by the csrf helper to set a cookie; fragments
	// get none, so those helpers fail instead of issuing one.
	body, err := fragment.execute(nil, request, name, data)
	if err != nil {
		return "", err
	}
	// #nosec G203 -- html/template output, escaped for its context.
	return template.HTML(body.String()), nil
}

// Pages returns the names of the renderable pages, sorted.
func (r *Renderer) Pages() []string {
	names := make([]string, 0, len(r.pages))
	for name := range r.pages {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

var errNoRequest = errors.New("this helper needs RenderRequest")

// helperFuncs returns the built-in helpers. With a nil state (parse time)
// the request helpers are placeholders that are replaced on every clone.
func helperFuncs(state *renderState, setup *setup) template.FuncMap {
	// request is the current request; fragments rendered for a broadcast have
	// one without a response writer.
	currentRequest := func() (*http.Request, error) {
		if state == nil || state.request == nil {
			return nil, errNoRequest
		}
		return state.request, nil
	}
	request := func() (http.ResponseWriter, *http.Request, error) {
		if state == nil || state.request == nil || state.writer == nil {
			return nil, nil, errNoRequest
		}
		return state.writer, state.request, nil
	}
	token := func() (string, error) {
		w, r, err := request()
		if err != nil {
			return "", err
		}
		return csrf.Token(w, r)
	}
	return template.FuncMap{
		"assetPath":               setup.assetPath,
		"stylesheetTag":           setup.stylesheetTag,
		"javascriptImportmapTags": setup.javascriptImportmapTags,
		"turboStreamFrom": func(name string) (template.HTML, error) {
			if err := setup.needStreams("turboStreamFrom"); err != nil {
				return "", err
			}
			r, err := currentRequest()
			if err != nil {
				return "", err
			}
			tenant, err := tenancy.Require(r.Context())
			if err != nil {
				return "", fmt.Errorf("turboStreamFrom %q: %w (use turboStreamFromGlobal for a channel shared by every tenant)", name, err)
			}
			channel, err := broadcast.ForTenant(tenant.ID, name)
			if err != nil {
				return "", fmt.Errorf("turboStreamFrom: %w", err)
			}
			return setup.streamSource(channel)
		},
		"turboStreamFromGlobal": func(name string) (template.HTML, error) {
			if err := setup.needStreams("turboStreamFromGlobal"); err != nil {
				return "", err
			}
			if _, err := currentRequest(); err != nil {
				return "", err
			}
			channel, err := broadcast.Global(name)
			if err != nil {
				return "", fmt.Errorf("turboStreamFromGlobal: %w", err)
			}
			return setup.streamSource(channel)
		},
		"csrfToken": token,
		"csrfField": func() (template.HTML, error) {
			value, err := token()
			if err != nil {
				return "", err
			}
			// #nosec G203 -- the token is canonical base64url (csrf.Token validates or generates it) and is escaped again here.
			return template.HTML(`<input type="hidden" name="` + csrf.FormField + `" value="` + template.HTMLEscapeString(value) + `">`), nil
		},
		"csrfMeta": func() (template.HTML, error) {
			value, err := token()
			if err != nil {
				return "", err
			}
			// #nosec G203 -- the token is canonical base64url (csrf.Token validates or generates it) and is escaped again here.
			return template.HTML(`<meta name="csrf-param" content="` + csrf.FormField + `"><meta name="csrf-token" content="` + template.HTMLEscapeString(value) + `">`), nil
		},
		"flashes": func() ([]web.FlashMessage, error) {
			_, r, err := request()
			if err != nil {
				return nil, err
			}
			return web.Flashes(r.Context()), nil
		},
		"flash": func(kind string) (string, error) {
			_, r, err := request()
			if err != nil {
				return "", err
			}
			return web.FlashText(r.Context(), web.FlashKind(kind)), nil
		},
		"can": func(permission string) (bool, error) {
			required, err := auth.NewPermission(permission)
			if err != nil {
				return false, fmt.Errorf("can: %w", err)
			}
			if state == nil || state.request == nil {
				return true, nil
			}
			principal, ok := auth.FromContext(state.request.Context())
			return !ok || principal.Can(required), nil
		},
		"signedIn": func() bool {
			if state == nil || state.request == nil {
				return false
			}
			_, ok := auth.FromContext(state.request.Context())
			return ok
		},
		"methodField": methodField,
		"domID":       domID,
		"dict":        dict,
		"t":           translate(state, setup),
		"l":           localize(state, setup),
		"number":      number(state, setup),
		"locale": func() (string, error) {
			if state == nil || state.probing {
				return i18n.DefaultLocale, nil
			}
			_, locale, err := translation(state, setup)
			return locale, err
		},
	}
}

var errNoTranslator = errors.New("this helper needs i18n.Translator.Middleware in front of the handler, or views.Options.I18n")

// translation returns the translator and locale of the current request, or
// Options.I18n and its default locale.
func translation(state *renderState, setup *setup) (*i18n.Translator, string, error) {
	if state != nil && state.request != nil {
		if translator, locale, ok := i18n.FromContext(state.request.Context()); ok {
			return translator, locale, nil
		}
	}
	if setup.translator != nil {
		return setup.translator, setup.translator.DefaultLocale(), nil
	}
	return nil, "", errNoTranslator
}

// translate is the t helper. It returns template.HTML for _html keys (their
// markup is trusted, their arguments escaped) and a string, which the
// template escapes for its context, for every other key.
func translate(state *renderState, setup *setup) func(string, ...any) (any, error) {
	return func(key string, args ...any) (any, error) {
		if len(args)%2 != 0 {
			return nil, &setupError{fmt.Sprintf("t %q: arguments must be name/value pairs", key)}
		}
		for i := 0; i < len(args); i += 2 {
			if _, ok := args[i].(string); !ok {
				return nil, &setupError{fmt.Sprintf("t %q: argument name %d is %T, not a string", key, i/2+1, args[i])}
			}
		}
		if state == nil || state.probing {
			return key, nil
		}
		translator, locale, err := translation(state, setup)
		if err != nil {
			return nil, fmt.Errorf("t %q: %w", key, err)
		}
		if i18n.IsHTMLKey(key) {
			return translator.HTML(locale, key, args...), nil
		}
		return translator.Translate(locale, key, args...), nil
	}
}

// localize is the l helper: l .Time, l .Time "short", l .Time "date.formats.long".
func localize(state *renderState, setup *setup) func(time.Time, ...string) (string, error) {
	return func(value time.Time, format ...string) (string, error) {
		if len(format) > 1 {
			return "", errors.New("l: takes a time and at most one format name")
		}
		if state == nil || state.probing {
			return "", nil
		}
		translator, locale, err := translation(state, setup)
		if err != nil {
			return "", fmt.Errorf("l: %w", err)
		}
		return translator.FormatTime(locale, value, strings.Join(format, "")), nil
	}
}

// number is the number helper: number .Count, number .Total 2.
func number(state *renderState, setup *setup) func(any, ...int) (string, error) {
	return func(value any, precision ...int) (string, error) {
		if len(precision) > 1 {
			return "", errors.New("number: takes a number and at most one precision")
		}
		if state == nil || state.probing {
			return "", nil
		}
		translator, locale, err := translation(state, setup)
		if err != nil {
			return "", fmt.Errorf("number: %w", err)
		}
		digits := -1
		if len(precision) == 1 {
			digits = precision[0]
		}
		text, ok := translator.FormatNumber(locale, value, digits)
		if !ok {
			return "", fmt.Errorf("number: %T is not a number", value)
		}
		return text, nil
	}
}

// methodField returns only constant markup, chosen by the method name.
func methodField(method string) (template.HTML, error) {
	switch strings.ToUpper(method) {
	case http.MethodPut:
		return `<input type="hidden" name="_method" value="PUT">`, nil
	case http.MethodPatch:
		return `<input type="hidden" name="_method" value="PATCH">`, nil
	case http.MethodDelete:
		return `<input type="hidden" name="_method" value="DELETE">`, nil
	}
	return "", fmt.Errorf("methodField: %q is not PUT, PATCH or DELETE", method)
}

// domID returns a plain string; the template escapes it for its context.
func domID(prefix string, id any) (string, error) { return turbo.DomID(prefix, id) }

func dict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, errors.New("dict: needs key and value pairs")
	}
	values := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %d is %T, not a string", i/2, pairs[i])
		}
		values[key] = pairs[i+1]
	}
	return values, nil
}
