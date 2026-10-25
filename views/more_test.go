package views_test

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rocketinsidecorp/forge-go/views"
)

// failingFS makes one named file unreadable, or the walk itself fail.
type failingFS struct {
	fstest.MapFS
	unreadable string
	walkFails  bool
}

func (f failingFS) Open(name string) (fs.File, error) {
	if name == f.unreadable {
		return nil, errors.New("disk error")
	}
	return f.MapFS.Open(name)
}

func (f failingFS) ReadFile(name string) ([]byte, error) {
	if name == f.unreadable {
		return nil, errors.New("disk error")
	}
	return f.MapFS.ReadFile(name)
}

func (f failingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if f.walkFails && name == "." {
		return nil, errors.New("directory error")
	}
	return f.MapFS.ReadDir(name)
}

func TestLoadReportsUnreadableTreesAndBrokenTemplates(t *testing.T) {
	t.Parallel()
	page := fstest.MapFS{"page.gohtml": {Data: []byte(`hello`)}}
	if _, err := views.Load(failingFS{MapFS: page, unreadable: "page.gohtml"}, views.Options{}); err == nil || !strings.Contains(err.Error(), "read page.gohtml") {
		t.Errorf("unreadable file: %v", err)
	}
	if _, err := views.Load(failingFS{MapFS: page, walkFails: true}, views.Options{}); err == nil {
		t.Error("an unreadable directory was accepted")
	}
	for name, files := range map[string]fstest.MapFS{
		"broken partial":    {"posts/_row.gohtml": {Data: []byte(`{{.Broken`)}, "page.gohtml": {Data: []byte(`x`)}},
		"broken layout":     {"layouts/application.gohtml": {Data: []byte(`{{`)}, "page.gohtml": {Data: []byte(`x`)}},
		"escaping partial":  {"_row.gohtml": {Data: []byte(`<a href="{{.}}`)}, "page.gohtml": {Data: []byte(`x`)}},
		"page redefines":    {"layouts/application.gohtml": {Data: []byte(`{{block "content" .}}{{end}}`)}, "page.gohtml": {Data: []byte(`{{define "content"}}a{{end}}{{define "content"}}b{{end}}`)}},
		"odd t arguments":   {"page.gohtml": {Data: []byte(`{{t "key" "name"}}`)}},
		"t name not string": {"page.gohtml": {Data: []byte(`{{t "key" 1 2}}`)}},
	} {
		if _, err := views.Load(files, views.Options{}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// Hidden directories are skipped, so their templates never load.
	hidden := fstest.MapFS{"page.gohtml": {Data: []byte(`x`)}, ".git/bad.gohtml": {Data: []byte(`{{`)}}
	if _, err := views.Load(hidden, views.Options{}); err != nil {
		t.Errorf("a hidden directory was read: %v", err)
	}
}

func TestFlashHelperRendersTheKindsMessage(t *testing.T) {
	t.Parallel()
	renderer := load(t, fstest.MapFS{"page.gohtml": {Data: []byte(`[{{flash "notice"}}][{{len flashes}}]`)}}, views.Options{})
	recorder := serve(t, renderer, "page", http.StatusOK, nil)
	if recorder.Body.String() != "[][0]" {
		t.Errorf("body = %q", recorder.Body.String())
	}
	// Without a request the helper fails the render instead of guessing.
	if err := renderer.Render(httptest.NewRecorder(), "page", http.StatusOK, nil); err == nil {
		t.Error("flash rendered without a request")
	}
}

func TestLocalizationHelpersRejectBadArgumentsAndNeedATranslator(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, time.February, 3, 0, 0, 0, 0, time.UTC)
	render := func(source string, opts views.Options, data any) error {
		renderer := load(t, fstest.MapFS{"page.gohtml": {Data: []byte(source)}}, opts)
		recorder := httptest.NewRecorder()
		err := renderer.Render(recorder, "page", http.StatusOK, data)
		if err != nil && recorder.Body.Len() != 0 {
			t.Errorf("%s: a failed render wrote %q", source, recorder.Body.String())
		}
		return err
	}
	withTranslator := views.Options{I18n: translator(t)}
	for source, opts := range map[string]views.Options{
		`{{l .When}}`:                {},
		`{{number .Total}}`:          {},
		`{{t "posts.title"}}`:        {},
		`{{locale}}`:                 {},
		`{{l .When "short" "long"}}`: withTranslator,
		`{{number .Total 1 2}}`:      withTranslator,
		`{{number .Name}}`:           withTranslator,
	} {
		if err := render(source, opts, map[string]any{"When": when, "Total": 1.5, "Name": "text"}); err == nil {
			t.Errorf("%s rendered", source)
		}
	}
	var body strings.Builder
	renderer := load(t, fstest.MapFS{"page.gohtml": {Data: []byte(`{{number .Total 2}}|{{l .When "short"}}|{{locale}}|{{t "posts.title"}}`)}}, withTranslator)
	recorder := httptest.NewRecorder()
	if err := renderer.Render(recorder, "page", http.StatusOK, map[string]any{"When": when, "Total": 1234.5}); err != nil {
		t.Fatal(err)
	}
	body.WriteString(recorder.Body.String())
	if !strings.Contains(body.String(), "1,234.50") || !strings.Contains(body.String(), "Feb 3") || !strings.Contains(body.String(), "|en|Posts") {
		t.Errorf("body = %q", body.String())
	}
}

func TestStreamHelpersRejectBadChannelsAndMissingContext(t *testing.T) {
	t.Parallel()
	opts := hotwireOptions(t)
	for name, source := range map[string]string{
		"bad tenant channel": `{{turboStreamFrom "Bad Name"}}`,
		"bad global channel": `{{turboStreamFromGlobal "Bad Name"}}`,
	} {
		renderer := load(t, fstest.MapFS{"page.gohtml": {Data: []byte(source)}}, opts)
		request, _ := tenantRequest(t)
		recorder := httptest.NewRecorder()
		if err := renderer.RenderRequest(recorder, request, "page", http.StatusOK, nil); err == nil || recorder.Body.Len() != 0 {
			t.Errorf("%s: err=%v body=%q", name, err, recorder.Body.String())
		}
	}
	// Without a request the global helper fails like the tenant one.
	renderer := load(t, fstest.MapFS{"page.gohtml": {Data: []byte(`{{turboStreamFromGlobal "banner"}}`)}}, opts)
	if err := renderer.Render(httptest.NewRecorder(), "page", http.StatusOK, nil); err == nil {
		t.Error("a stream source rendered without a request")
	}
	// A tenant stream needs a tenant, and says to use the global one.
	tenantless := load(t, fstest.MapFS{"page.gohtml": {Data: []byte(`{{turboStreamFrom "posts"}}`)}}, opts)
	err := tenantless.RenderRequest(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "page", http.StatusOK, nil)
	if err == nil || !strings.Contains(err.Error(), "turboStreamFromGlobal") {
		t.Errorf("tenantless stream: %v", err)
	}
}

func TestMethodFieldCoversEveryVerb(t *testing.T) {
	t.Parallel()
	renderer := load(t, fstest.MapFS{"page.gohtml": {Data: []byte(`{{methodField "put"}}{{methodField "PATCH"}}`)}}, views.Options{})
	recorder := httptest.NewRecorder()
	if err := renderer.Render(recorder, "page", http.StatusOK, nil); err != nil {
		t.Fatal(err)
	}
	if body := recorder.Body.String(); !strings.Contains(body, `value="PUT"`) || !strings.Contains(body, `value="PATCH"`) {
		t.Errorf("body = %q", body)
	}
}
