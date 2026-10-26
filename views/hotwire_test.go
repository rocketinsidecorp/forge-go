package views_test

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rocketinsidecorp/forge-go/assets"
	"github.com/rocketinsidecorp/forge-go/broadcast"
	"github.com/rocketinsidecorp/forge-go/hotwire"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/views"
)

var hotwireSite = fstest.MapFS{
	"layouts/application.gohtml": {Data: []byte(`<!doctype html><html><head><title>App</title>{{stylesheetTag "application"}}{{javascriptImportmapTags "application"}}</head>` +
		`<body><img src="{{assetPath "logo.png"}}"><main>{{block "content" .}}{{end}}</main></body></html>`)},
	"posts/_post.gohtml":   {Data: []byte(`<li id="{{domID "post" .ID}}">{{.Title}}</li>`)},
	"posts/_secret.gohtml": {Data: []byte(`<form>{{csrfField}}</form>`)},
	"posts/index.gohtml":   {Data: []byte(`{{define "content"}}{{turboStreamFrom "posts"}}{{turboStreamFromGlobal "announcements"}}<ul id="posts">{{range .}}{{template "posts/_post" .}}{{end}}</ul>{{end}}`)},
}

func hotwireOptions(t *testing.T) views.Options {
	t.Helper()
	set, err := assets.New(assets.Options{},
		fstest.MapFS{
			"application.css": {Data: []byte("body{}")},
			"application.js":  {Data: []byte(`import "@hotwired/turbo"`)},
			"logo.png":        {Data: []byte("png")},
		},
		hotwire.Files(),
	)
	if err != nil {
		t.Fatal(err)
	}
	imports, err := assets.NewImportMap(set, hotwire.Pins()...)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := broadcast.NewSigner(broadcast.SignerOptions{Key: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	return views.Options{Assets: set, ImportMap: imports, StyleHashes: hotwire.StyleHashes(), Streams: signer}
}

func tenantRequest(t *testing.T) (*http.Request, uuid.UUID) {
	t.Helper()
	tenant := uuid.MustNew()
	ctx, err := tenancy.WithContext(context.Background(), tenancy.Tenant{ID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(http.MethodGet, "https://app.example.com/posts", nil).WithContext(ctx), tenant
}

func TestAssetHelpersRenderDigestedPathsAndTheImportMap(t *testing.T) {
	t.Parallel()
	opts := hotwireOptions(t)
	renderer := load(t, hotwireSite, opts)
	request, _ := tenantRequest(t)
	recorder := httptest.NewRecorder()
	if err := renderer.RenderRequest(recorder, request, "posts/index", http.StatusOK, nil); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	css, _ := opts.Assets.Path("application.css")
	js, _ := opts.Assets.Path("application.js")
	logo, _ := opts.Assets.Path("logo.png")
	turbo, _ := opts.Assets.Path(hotwire.TurboAsset)
	for _, want := range []string{
		`<link rel="stylesheet" href="` + css + `">`,
		`<img src="` + logo + `">`,
		string(opts.ImportMap.Tag()),
		`<link rel="modulepreload" href="` + turbo + `">`,
		`<script type="module" src="` + js + `"></script>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// The import map must precede every module script and preload.
	if strings.Index(body, `type="importmap"`) > strings.Index(body, `rel="modulepreload"`) || strings.Index(body, `type="importmap"`) > strings.Index(body, `type="module"`) {
		t.Errorf("the import map must come first:\n%s", body)
	}
	if strings.Contains(body, "/assets/application.css") {
		t.Error("an undigested asset URL was rendered")
	}
}

func TestPolicyAllowsTheImportMapByHashOnly(t *testing.T) {
	t.Parallel()
	opts := hotwireOptions(t)
	renderer := load(t, hotwireSite, opts)
	request, _ := tenantRequest(t)
	recorder := httptest.NewRecorder()
	if err := renderer.RenderRequest(recorder, request, "posts/index", http.StatusOK, nil); err != nil {
		t.Fatal(err)
	}
	policy := recorder.Header().Get("Content-Security-Policy")
	want := "script-src 'self' " + opts.ImportMap.CSPSource() + ";"
	if !strings.Contains(policy, want) {
		t.Fatalf("policy %q lacks %q", policy, want)
	}
	if !strings.Contains(policy, "style-src 'self' 'sha256-"+hotwire.TurboProgressBarStyleSHA256+"';") {
		t.Fatalf("policy %q lacks Turbo's progress bar style hash", policy)
	}
	if strings.Contains(policy, "unsafe-inline") || strings.Contains(policy, "unsafe-eval") || strings.Contains(policy, "nonce") {
		t.Fatalf("the policy must stay strict: %q", policy)
	}
	// The hash is exactly what a browser computes over the rendered script body.
	body := recorder.Body.String()
	match := regexp.MustCompile(`<script type="importmap">(.*?)</script>`).FindStringSubmatch(body)
	if len(match) < 2 || match[1] != opts.ImportMap.JSON() {
		t.Fatalf("rendered import map = %v", match)
	}
	// A policy without the options is the unchanged default.
	plain := load(t, fstest.MapFS{"a.gohtml": {Data: []byte("a")}}, views.Options{})
	recorder = httptest.NewRecorder()
	if err := plain.Render(recorder, "a", 0, nil); err != nil {
		t.Fatal(err)
	}
	if recorder.Header().Get("Content-Security-Policy") != views.DefaultContentSecurityPolicy {
		t.Fatalf("default policy changed: %q", recorder.Header().Get("Content-Security-Policy"))
	}
	// An explicit policy is used verbatim.
	custom := hotwireOptions(t)
	custom.ContentSecurityPolicy = "default-src 'none'"
	renderer = load(t, hotwireSite, custom)
	recorder = httptest.NewRecorder()
	if err := renderer.RenderRequest(recorder, request, "posts/index", 0, nil); err != nil {
		t.Fatal(err)
	}
	if recorder.Header().Get("Content-Security-Policy") != "default-src 'none'" {
		t.Fatalf("custom policy = %q", recorder.Header().Get("Content-Security-Policy"))
	}
}

func TestTurboStreamFromSignsTenantAndGlobalChannels(t *testing.T) {
	t.Parallel()
	opts := hotwireOptions(t)
	renderer := load(t, hotwireSite, opts)
	request, tenant := tenantRequest(t)
	recorder := httptest.NewRecorder()
	if err := renderer.RenderRequest(recorder, request, "posts/index", http.StatusOK, nil); err != nil {
		t.Fatal(err)
	}
	sources := regexp.MustCompile(`<turbo-stream-source src="(/streams\?stream=[A-Za-z0-9_.-]+)"></turbo-stream-source>`).FindAllStringSubmatch(recorder.Body.String(), -1)
	if len(sources) != 2 {
		t.Fatalf("sources = %v in:\n%s", sources, recorder.Body.String())
	}
	got := map[string]broadcast.Channel{}
	for _, source := range sources {
		channel, err := opts.Streams.Verify(strings.TrimPrefix(source[1], "/streams?stream="))
		if err != nil {
			t.Fatal(err)
		}
		got[channel.Name] = channel
	}
	if got["posts"].Tenant != tenant || got["posts"].IsGlobal() {
		t.Errorf("posts channel = %+v, want tenant %s", got["posts"], tenant)
	}
	if !got["announcements"].IsGlobal() {
		t.Errorf("announcements channel = %+v, want global", got["announcements"])
	}
}

// A tenant-scoped subscription without a tenant must fail the render rather
// than quietly fall back to a channel other tenants can read.
func TestTurboStreamFromFailsClosedWithoutATenant(t *testing.T) {
	t.Parallel()
	renderer := load(t, hotwireSite, hotwireOptions(t))
	request := httptest.NewRequest(http.MethodGet, "https://app.example.com/posts", nil)
	err := renderer.RenderRequest(httptest.NewRecorder(), request, "posts/index", http.StatusOK, nil)
	if err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("err = %v", err)
	}
	if err := renderer.Render(httptest.NewRecorder(), "posts/index", http.StatusOK, nil); err == nil {
		t.Fatal("rendering without a request must fail")
	}
}

func TestMisconfiguredHelpersStopLoad(t *testing.T) {
	t.Parallel()
	good := hotwireOptions(t)
	page := func(source string) fstest.MapFS {
		return fstest.MapFS{"a.gohtml": {Data: []byte(source)}}
	}
	for name, test := range map[string]struct {
		files fstest.MapFS
		opts  views.Options
	}{
		"assetPath without assets":    {page(`{{assetPath "a.css"}}`), views.Options{}},
		"stylesheetTag without":       {page(`{{stylesheetTag "a"}}`), views.Options{}},
		"importmap without assets":    {page(`{{javascriptImportmapTags "application"}}`), views.Options{}},
		"importmap without the map":   {page(`{{javascriptImportmapTags "application"}}`), views.Options{Assets: good.Assets}},
		"unknown asset":               {page(`{{assetPath "missing.css"}}`), good},
		"unknown stylesheet":          {page(`{{stylesheetTag "missing"}}`), good},
		"unknown entry":               {page(`{{javascriptImportmapTags "missing"}}`), good},
		"stream helper without":       {page(`{{turboStreamFrom "posts"}}`), views.Options{}},
		"global stream without":       {page(`{{turboStreamFromGlobal "posts"}}`), views.Options{}},
		"import map needs assets":     {page(`x`), views.Options{ImportMap: good.ImportMap}},
		"bad style hash":              {page(`x`), views.Options{StyleHashes: []string{"'unsafe-inline'"}}},
		"style hash injects a source": {page(`x`), views.Options{StyleHashes: []string{"AAAA; script-src *"}}},
		"streams path off site":       {page(`x`), views.Options{StreamsPath: "https://evil.example/streams"}},
		"streams path with query":     {page(`x`), views.Options{StreamsPath: "/streams?x=1"}},
	} {
		if _, err := views.Load(test.files, test.opts); err == nil {
			t.Errorf("%s: Load succeeded", name)
		}
	}
	if _, err := views.Load(page(`{{assetPath "application.css"}}`), good); err != nil {
		t.Errorf("a valid asset reference failed: %v", err)
	}
}

func TestFragmentRendersAPartialEscaped(t *testing.T) {
	t.Parallel()
	renderer := load(t, hotwireSite, hotwireOptions(t))
	id := uuid.MustNew()
	html, err := renderer.Fragment(nil, "posts/_post", struct {
		ID    uuid.UUID
		Title string
	}{id, `</li><script>alert(1)</script>`})
	if err != nil {
		t.Fatal(err)
	}
	want := `<li id="post_` + id.String() + `">&lt;/li&gt;&lt;script&gt;alert(1)&lt;/script&gt;</li>`
	if html != template.HTML(want) {
		t.Fatalf("Fragment = %s, want %s", html, want)
	}
	if _, err := renderer.Fragment(nil, "posts/index", nil); err == nil {
		t.Error("a page rendered as a fragment")
	}
	if _, err := renderer.Fragment(nil, "posts/_missing", nil); err == nil {
		t.Error("a missing partial rendered")
	}
	if _, err := (*views.Renderer)(nil).Fragment(nil, "x", nil); err == nil {
		t.Error("a nil renderer rendered")
	}
	flat, _ := views.New(fstest.MapFS{"a.gohtml": {Data: []byte("a")}})
	if _, err := flat.Fragment(nil, "a", nil); err == nil {
		t.Error("a flat renderer rendered a fragment")
	}
}

// Fragments are broadcast to every subscriber, so a CSRF token (bound to the
// publishing browser) must never be rendered into one, even with a request.
func TestFragmentsNeverCarryRequestSpecificTokens(t *testing.T) {
	t.Parallel()
	renderer := load(t, hotwireSite, hotwireOptions(t))
	request, _ := tenantRequest(t)
	if html, err := renderer.Fragment(request, "posts/_secret", nil); err == nil {
		t.Fatalf("a fragment rendered a CSRF token: %s", html)
	}
	if html, err := renderer.Fragment(nil, "posts/_secret", nil); err == nil {
		t.Fatalf("a fragment rendered a CSRF token without a request: %s", html)
	}
}

func TestFragmentsAreSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	renderer := load(t, hotwireSite, hotwireOptions(t))
	done := make(chan struct{})
	for range 16 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 50 {
				if _, err := renderer.Fragment(nil, "posts/_post", struct {
					ID    uuid.UUID
					Title string
				}{uuid.MustNew(), "x"}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for range 16 {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("timed out")
		}
	}
}
