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

	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/uuid"
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
	// ContentSecurityPolicy replaces DefaultContentSecurityPolicy.
	ContentSecurityPolicy string
}

// page is one renderable template set: the layouts, the partials and one
// page file. pristine is never executed, so it can always be cloned; the
// pool holds escaped clones whose request helpers read their own state.
type page struct {
	entry    string
	pristine *template.Template
	pool     sync.Pool
}

type instance struct {
	templates *template.Template
	state     *renderState
}

type renderState struct {
	writer  http.ResponseWriter
	request *http.Request
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
// reported here rather than at the first request.
//
// Pages may use these helpers:
//
//	csrfField          hidden authenticity_token input for a form
//	csrfMeta           csrf-param and csrf-token meta tags
//	csrfToken          the token itself, for data attributes
//	methodField "PATCH" hidden _method input (PUT, PATCH or DELETE)
//	flashes            the incoming web.FlashMessage values
//	flash "notice"     the text of one incoming flash message
//	domID "post" .ID   an element id: post_<id>, or new_post for a zero UUID
//	dict "k" v ...     a map, for passing several values to a partial
//
// The csrf and flash helpers need RenderRequest and the csrf.Protection and
// web.Flash middleware; anywhere else they fail the render.
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
	funcs := helperFuncs(nil)
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
		csp = DefaultContentSecurityPolicy
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
	renderer := &Renderer{pages: make(map[string]*page, len(pages)), csp: csp}
	for _, file := range pages {
		parsed, err := parsePage(base, funcs, file, layout)
		if err != nil {
			return nil, err
		}
		renderer.pages[file.name] = parsed
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

func parsePage(base *template.Template, funcs template.FuncMap, file source, layout string) (*page, error) {
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
	parsed := &page{entry: entry, pristine: pristine}
	// Escape a throwaway clone now: html/template escapes lazily, and a
	// template it cannot escape safely should stop the application at
	// startup. Execution errors from the missing data are expected.
	probe, err := parsed.newInstance()
	if err != nil {
		return nil, err
	}
	var escapeErr *template.Error
	if err := probe.templates.ExecuteTemplate(io.Discard, entry, nil); errors.As(err, &escapeErr) {
		return nil, fmt.Errorf("views: %s: %w", file.name, err)
	}
	return parsed, nil
}

func (p *page) newInstance() (*instance, error) {
	clone, err := p.pristine.Clone()
	if err != nil {
		return nil, fmt.Errorf("views: clone templates: %w", err)
	}
	state := &renderState{}
	return &instance{templates: clone.Funcs(helperFuncs(state)), state: state}, nil
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
	current, err := page.acquire()
	if err != nil {
		return err
	}
	current.state.writer, current.state.request = w, request
	var body bytes.Buffer
	err = current.templates.ExecuteTemplate(&body, page.entry, data)
	current.state.writer, current.state.request = nil, nil
	page.pool.Put(current)
	if err != nil {
		return fmt.Errorf("views: execute %q: %w", name, err)
	}
	w.Header().Set("Content-Security-Policy", r.csp)
	return writeHTML(w, status, body.Bytes())
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
func helperFuncs(state *renderState) template.FuncMap {
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
		"methodField": methodField,
		"domID":       domID,
		"dict":        dict,
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

var (
	domPrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	domIDPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// domID returns a plain string; the template escapes it for its context.
func domID(prefix string, id any) (string, error) {
	if !domPrefixPattern.MatchString(prefix) {
		return "", fmt.Errorf("domID: invalid prefix %q", prefix)
	}
	var value string
	switch typed := id.(type) {
	case uuid.UUID:
		if typed == (uuid.UUID{}) {
			return "new_" + prefix, nil
		}
		value = typed.String()
	case string:
		value = typed
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		value = fmt.Sprint(typed)
	default:
		return "", fmt.Errorf("domID: unsupported id type %T", id)
	}
	if !domIDPattern.MatchString(value) {
		return "", errors.New("domID: id must be letters, digits, '_' or '-'")
	}
	return prefix + "_" + value, nil
}

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
