package assets_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rocketinsidecorp/forge-go/assets"
)

func digest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func newSet(t *testing.T) *assets.Set {
	t.Helper()
	set, err := assets.New(assets.Options{},
		fstest.MapFS{
			"application.css":       {Data: []byte("body{}")},
			"css/print.min.css":     {Data: []byte("p{}")},
			"logo.svg":              {Data: []byte("<svg/>")},
			".hidden/secret.txt":    {Data: []byte("no")},
			".keep":                 {Data: nil},
			"controllers/hello.js":  {Data: []byte("export default 1")},
			"controllers/index.js":  {Data: []byte("import './hello'")},
			"controllers/notes.txt": {Data: []byte("n")},
		},
		fstest.MapFS{"vendor/turbo.js": {Data: []byte("turbo")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func get(handler http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestPathsAreContentDigested(t *testing.T) {
	t.Parallel()
	set := newSet(t)
	want := "/assets/application-" + digest("body{}")[:16] + ".css"
	if got, ok := set.Path("application.css"); !ok || got != want {
		t.Fatalf("Path = %q, %v; want %q", got, ok, want)
	}
	if got, _ := set.Path("css/print.min.css"); got != "/assets/css/print.min-"+digest("p{}")[:16]+".css" {
		t.Fatalf("nested path = %q", got)
	}
	if _, ok := set.Path("missing.css"); ok {
		t.Fatal("unknown asset resolved")
	}
	if _, ok := set.Path(".hidden/secret.txt"); ok {
		t.Fatal("hidden files must be skipped")
	}
	if full, _ := set.Digest("application.css"); full != digest("body{}") {
		t.Fatalf("Digest = %q", full)
	}
	if set.Len() != 7 {
		t.Fatalf("Len = %d: %v", set.Len(), set.Logicals())
	}
}

func TestHandlerServesOnlyTheTable(t *testing.T) {
	t.Parallel()
	set := newSet(t)
	handler := set.Handler()
	url, _ := set.Path("application.css")

	ok := get(handler, http.MethodGet, url, nil)
	if ok.Code != http.StatusOK || ok.Body.String() != "body{}" {
		t.Fatalf("GET status=%d body=%q", ok.Code, ok.Body.String())
	}
	for key, want := range map[string]string{
		"Content-Type":           "text/css; charset=utf-8",
		"Cache-Control":          "public, max-age=31536000, immutable",
		"X-Content-Type-Options": "nosniff",
		"Etag":                   `"` + digest("body{}") + `"`,
	} {
		if got := ok.Header().Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if ok.Header().Get("Set-Cookie") != "" || ok.Header().Get("Last-Modified") != "" {
		t.Errorf("unexpected headers: %v", ok.Header())
	}

	head := get(handler, http.MethodHead, url, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "6" {
		t.Fatalf("HEAD status=%d len=%d headers=%v", head.Code, head.Body.Len(), head.Header())
	}

	notModified := get(handler, http.MethodGet, url, map[string]string{"If-None-Match": `"` + digest("body{}") + `"`})
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 || notModified.Header().Get("Cache-Control") == "" {
		t.Fatalf("conditional GET status=%d headers=%v", notModified.Code, notModified.Header())
	}
	if stale := get(handler, http.MethodGet, url, map[string]string{"If-None-Match": `"other"`}); stale.Code != http.StatusOK {
		t.Fatalf("mismatched ETag status=%d", stale.Code)
	}

	if post := get(handler, http.MethodPost, url, nil); post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST status=%d headers=%v", post.Code, post.Header())
	}

	svgURL, _ := set.Path("logo.svg")
	if svg := get(handler, http.MethodGet, svgURL, nil); !strings.Contains(svg.Header().Get("Content-Security-Policy"), "sandbox") || svg.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("svg headers=%v", svg.Header())
	}
	jsURL, _ := set.Path("vendor/turbo.js")
	if js := get(handler, http.MethodGet, jsURL, nil); js.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("js content type = %q", js.Header().Get("Content-Type"))
	}
}

func TestHandlerRefusesEverythingElse(t *testing.T) {
	t.Parallel()
	handler := newSet(t).Handler()
	digested := "application-" + digest("body{}")[:16] + ".css"
	for _, target := range []string{
		"/assets/application.css",     // undigested logical name
		"/assets/",                    // directory listing
		"/assets",                     // the prefix itself
		"/assets/css/",                // nested directory
		"/assets/../go.mod",           // traversal
		"/assets/%2e%2e/go.mod",       // encoded traversal
		"/assets/css%2Fprint.min.css", // encoded separator
		"/assets/" + digested + "/",   // trailing slash
		"/assets//" + digested,        // empty segment
		"/other/" + digested,          // other prefix
		"/assets/" + strings.ToUpper(digested),
		"/assets/application-0000000000000000.css", // wrong digest
		"/assets/.hidden/secret.txt",
	} {
		if recorder := get(handler, http.MethodGet, target, nil); recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s status=%d, want 404", target, recorder.Code)
		}
	}
}

func TestNewValidatesTheTable(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		options assets.Options
		sources []fstest.MapFS
	}{
		"unserved extension": {sources: []fstest.MapFS{{"page.html": {Data: []byte("x")}}}},
		"no extension":       {sources: []fstest.MapFS{{"README": {Data: []byte("x")}}}},
		"space in name":      {sources: []fstest.MapFS{{"a b.css": {Data: []byte("x")}}}},
		"non-ASCII name":     {sources: []fstest.MapFS{{"é.css": {Data: []byte("x")}}}},
		"duplicate":          {sources: []fstest.MapFS{{"a.css": {Data: []byte("x")}}, {"a.css": {Data: []byte("y")}}}},
		"relative prefix":    {options: assets.Options{Prefix: "assets"}, sources: []fstest.MapFS{{"a.css": {Data: []byte("x")}}}},
		"root prefix":        {options: assets.Options{Prefix: "/"}, sources: []fstest.MapFS{{"a.css": {Data: []byte("x")}}}},
		"trailing slash":     {options: assets.Options{Prefix: "/assets/"}, sources: []fstest.MapFS{{"a.css": {Data: []byte("x")}}}},
		"traversal prefix":   {options: assets.Options{Prefix: "/../x"}, sources: []fstest.MapFS{{"a.css": {Data: []byte("x")}}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sources := make([]fs.FS, 0, len(test.sources))
			for _, source := range test.sources {
				sources = append(sources, source)
			}
			if _, err := assets.New(test.options, sources...); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if _, err := assets.New(assets.Options{}, nil); err == nil {
		t.Fatal("nil source accepted")
	}
}

func TestCustomPrefix(t *testing.T) {
	t.Parallel()
	set, err := assets.New(assets.Options{Prefix: "/static/v1"}, fstest.MapFS{"a.css": {Data: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	url, _ := set.Path("a.css")
	if !strings.HasPrefix(url, "/static/v1/a-") {
		t.Fatalf("url = %q", url)
	}
	if recorder := get(set.Handler(), http.MethodGet, url, nil); recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestImportMap(t *testing.T) {
	t.Parallel()
	set := newSet(t)
	turbo, _ := set.Path("vendor/turbo.js")
	hello, _ := set.Path("controllers/hello.js")
	index, _ := set.Path("controllers/index.js")

	pins := append([]assets.Pin{{Specifier: "@hotwired/turbo", Logical: "vendor/turbo.js"}}, assets.PinAllFrom(set, "controllers")...)
	pinned := map[string]bool{}
	for _, pin := range pins {
		pinned[pin.Specifier] = true
	}
	for _, want := range []string{"controllers", "controllers/index", "controllers/hello"} {
		if !pinned[want] {
			t.Errorf("PinAllFrom did not pin %q: %v", want, pins)
		}
	}
	if pinned["controllers/notes"] || len(pins) != 4 {
		t.Errorf("PinAllFrom pinned a non-script: %v", pins)
	}

	imports, err := assets.NewImportMap(set, pins...)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"imports":{"@hotwired/turbo":"` + turbo + `","controllers":"` + index + `","controllers/hello":"` + hello + `","controllers/index":"` + index + `"}}`
	if imports.JSON() != want {
		t.Fatalf("JSON =\n%s\nwant\n%s", imports.JSON(), want)
	}
	if string(imports.Tag()) != `<script type="importmap">`+want+`</script>` {
		t.Fatalf("Tag = %s", imports.Tag())
	}
	// The hash must be exactly what a browser computes over the script body.
	sum := sha256.Sum256([]byte(imports.JSON()))
	if imports.SHA256() != base64.StdEncoding.EncodeToString(sum[:]) || imports.CSPSource() != "'sha256-"+imports.SHA256()+"'" {
		t.Fatalf("hash %q / source %q", imports.SHA256(), imports.CSPSource())
	}
	if urls := imports.URLs(); len(urls) != 3 || urls[0] != hello {
		t.Fatalf("URLs = %v", urls)
	}
}

func TestImportMapIsScriptSafeAndStrict(t *testing.T) {
	t.Parallel()
	set := newSet(t)
	imports, err := assets.NewImportMap(set,
		assets.Pin{Specifier: "</script><script>alert(1)//", Logical: "vendor/turbo.js"},
		assets.Pin{Specifier: "a&b ", Logical: "controllers/hello.js"},
	)
	if err != nil {
		t.Fatal(err)
	}
	tag := string(imports.Tag())
	if strings.Count(tag, "</script>") != 1 || strings.ContainsAny(imports.JSON(), "<>& ") {
		t.Fatalf("map is not script-safe: %s", tag)
	}

	for name, pins := range map[string][]assets.Pin{
		"unknown asset":     {{Specifier: "x", Logical: "missing.js"}},
		"duplicate":         {{Specifier: "x", Logical: "vendor/turbo.js"}, {Specifier: "x", Logical: "controllers/hello.js"}},
		"empty specifier":   {{Specifier: "", Logical: "vendor/turbo.js"}},
		"control character": {{Specifier: "a\nb", Logical: "vendor/turbo.js"}},
		"too long":          {{Specifier: strings.Repeat("x", 257), Logical: "vendor/turbo.js"}},
	} {
		if _, err := assets.NewImportMap(set, pins...); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := assets.NewImportMap(nil); err == nil {
		t.Error("nil set accepted")
	}
}
