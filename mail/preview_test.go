package mail_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

// previewConfig is a configuration that explicitly opted in to previews.
func previewConfig(env config.Environment) config.Config {
	cfg := config.Default()
	cfg.Environment = env
	cfg.Mail.Preview = true
	return cfg
}

func previewHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, err := forgemail.NewPreviewHandler(previewConfig(config.Development), "/_mail",
		forgemail.Preview{Name: "welcome", Description: "Sent after sign-up <b>", Build: func(context.Context) (forgemail.Message, error) {
			msg := baseMessage()
			msg.Subject = `Welcome <script>alert(1)</script>`
			msg.Text = "plain <b>text</b>"
			msg.HTML = `<h1>Hello</h1><script>alert(1)</script><img src="cid:logo">`
			msg.Cc = []forgemail.Address{{Email: "cc@example.org"}}
			msg.Bcc = []forgemail.Address{{Email: "bcc@example.org"}}
			msg.Attachments = []forgemail.Attachment{{Filename: "logo.png", ContentType: "image/png", Data: []byte("PNGDATA"), ContentID: "logo"}}
			return msg, nil
		}},
		forgemail.Preview{Name: "broken", Build: func(context.Context) (forgemail.Message, error) {
			msg := baseMessage()
			msg.Subject = "x\r\nBcc: evil@example.net"
			return msg, nil
		}},
		forgemail.Preview{Name: "failing", Build: func(context.Context) (forgemail.Message, error) {
			return forgemail.Message{}, errors.New("fixture missing")
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func get(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestPreviewIsRefusedOutsideDevelopment(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.Production, config.Staging, "", "unknown"} {
		if _, err := forgemail.NewPreviewHandler(previewConfig(env), "/_mail"); err == nil {
			t.Errorf("preview created in %q", env)
		}
	}
	for _, env := range []config.Environment{config.Development, config.Test} {
		if _, err := forgemail.NewPreviewHandler(previewConfig(env), "/_mail"); err != nil {
			t.Errorf("preview refused in %q: %v", env, err)
		}
	}
}

func TestPreviewValidatesRegistration(t *testing.T) {
	t.Parallel()
	build := func(context.Context) (forgemail.Message, error) { return baseMessage(), nil }
	for name, tc := range map[string]struct {
		prefix   string
		previews []forgemail.Preview
	}{
		"prefix without slash":   {"_mail", nil},
		"prefix trailing slash":  {"/_mail/", nil},
		"prefix with query":      {"/_mail?x", nil},
		"empty name":             {"/_mail", []forgemail.Preview{{Build: build}}},
		"uppercase name":         {"/_mail", []forgemail.Preview{{Name: "Welcome", Build: build}}},
		"path in name":           {"/_mail", []forgemail.Preview{{Name: "a/b", Build: build}}},
		"traversal in name":      {"/_mail", []forgemail.Preview{{Name: "..", Build: build}}},
		"missing build":          {"/_mail", []forgemail.Preview{{Name: "x"}}},
		"duplicate":              {"/_mail", []forgemail.Preview{{Name: "x", Build: build}, {Name: "x", Build: build}}},
		"html in name":           {"/_mail", []forgemail.Preview{{Name: "<b>", Build: build}}},
		"newline in name":        {"/_mail", []forgemail.Preview{{Name: "a\nb", Build: build}}},
		"name too long":          {"/_mail", []forgemail.Preview{{Name: strings.Repeat("a", 65), Build: build}}},
		"leading hyphen in name": {"/_mail", []forgemail.Preview{{Name: "-x", Build: build}}},
	} {
		if _, err := forgemail.NewPreviewHandler(previewConfig(config.Development), tc.prefix, tc.previews...); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestPreviewIndexAndPage(t *testing.T) {
	t.Parallel()
	handler := previewHandler(t)

	index := get(handler, http.MethodGet, "/_mail/")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), `href="/_mail/welcome"`) {
		t.Fatalf("index = %d %s", index.Code, index.Body.String())
	}
	if strings.Contains(index.Body.String(), "<b>") {
		t.Fatal("description was not escaped")
	}
	if get(handler, http.MethodGet, "/_mail").Code != http.StatusOK {
		t.Fatal("prefix without trailing slash should list previews too")
	}

	page := get(handler, http.MethodGet, "/_mail/welcome")
	body := page.Body.String()
	if page.Code != http.StatusOK {
		t.Fatalf("page = %d %s", page.Code, body)
	}
	for _, want := range []string{"Forge App", "alice@example.org", "cc@example.org", "bcc@example.org", "logo.png (7 bytes), inline as cid:logo", `src="/_mail/welcome/html"`, "Message-ID", "&lt;script&gt;alert(1)&lt;/script&gt;", "plain &lt;b&gt;text&lt;/b&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("page misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Fatalf("page contains an unescaped script element:\n%s", body)
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Fatalf("page CSP = %q", csp)
	}
}

func TestPreviewHTMLTextRawAndInlineParts(t *testing.T) {
	t.Parallel()
	handler := previewHandler(t)

	html := get(handler, http.MethodGet, "/_mail/welcome/html")
	if html.Code != http.StatusOK || !strings.HasPrefix(html.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("html = %d %v", html.Code, html.Header())
	}
	if !strings.Contains(html.Body.String(), `src="/_mail/welcome/cid/logo"`) || strings.Contains(html.Body.String(), "cid:logo") {
		t.Fatalf("cid reference not rewritten: %s", html.Body.String())
	}
	csp := html.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "sandbox"} {
		if !strings.Contains(csp, want) {
			t.Errorf("HTML CSP %q misses %q", csp, want)
		}
	}
	if strings.Contains(csp, "script-src") || strings.Contains(csp, "'unsafe-eval'") {
		t.Errorf("HTML CSP allows scripts: %q", csp)
	}
	if html.Header().Get("X-Content-Type-Options") != "nosniff" || html.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", html.Header())
	}

	text := get(handler, http.MethodGet, "/_mail/welcome/text")
	if text.Body.String() != "plain <b>text</b>" || !strings.HasPrefix(text.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("text = %q %v", text.Body.String(), text.Header())
	}

	raw := get(handler, http.MethodGet, "/_mail/welcome/raw")
	parsed, err := mail.ReadMessage(strings.NewReader(raw.Body.String()))
	if err != nil || !strings.HasPrefix(parsed.Header.Get("Content-Type"), "multipart/mixed") && !strings.HasPrefix(parsed.Header.Get("Content-Type"), "multipart/alternative") {
		t.Fatalf("raw is not a MIME message: %v %v", err, parsed)
	}
	if !strings.HasPrefix(raw.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("raw must not be rendered as HTML: %v", raw.Header())
	}

	cid := get(handler, http.MethodGet, "/_mail/welcome/cid/logo")
	if cid.Body.String() != "PNGDATA" || cid.Header().Get("Content-Type") != "image/png" || !strings.Contains(cid.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("cid = %q %v", cid.Body.String(), cid.Header())
	}
	if get(handler, http.MethodGet, "/_mail/welcome/cid/other").Code != http.StatusNotFound {
		t.Fatal("unknown cid should be 404")
	}
}

func TestPreviewShowsValidationAndBuildErrors(t *testing.T) {
	t.Parallel()
	handler := previewHandler(t)
	broken := get(handler, http.MethodGet, "/_mail/broken")
	if broken.Code != http.StatusUnprocessableEntity || !strings.Contains(broken.Body.String(), "This message would not be sent") || !strings.Contains(broken.Body.String(), "Subject") {
		t.Fatalf("broken = %d %s", broken.Code, broken.Body.String())
	}
	for _, part := range []string{"html", "text", "raw"} {
		if rec := get(handler, http.MethodGet, "/_mail/broken/"+part); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s of a broken preview = %d", part, rec.Code)
		}
	}
	failing := get(handler, http.MethodGet, "/_mail/failing")
	if failing.Code != http.StatusUnprocessableEntity || !strings.Contains(failing.Body.String(), "fixture missing") {
		t.Fatalf("failing = %d %s", failing.Code, failing.Body.String())
	}
}

func TestPreviewRoutingAndMethods(t *testing.T) {
	t.Parallel()
	handler := previewHandler(t)
	for _, path := range []string{"/_mail/unknown", "/_mail/welcome/nothing", "/other/welcome", "/_mail/welcome/html/extra", "/_mail/../welcome", "/_mailx/welcome"} {
		if rec := get(handler, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := get(handler, method, "/_mail/welcome")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") == "" {
			t.Errorf("%s = %d", method, rec.Code)
		}
	}
	if rec := get(handler, http.MethodHead, "/_mail/welcome"); rec.Code != http.StatusOK {
		t.Errorf("HEAD = %d", rec.Code)
	}
}
