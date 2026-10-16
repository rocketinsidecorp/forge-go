package mail_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	forgemail "github.com/rocketinsidecorp/forge-go/mail"
	"github.com/rocketinsidecorp/forge-go/mail/mailtest"
)

type welcomeData struct {
	Name string
	URL  string
}

func welcomeFS() fstest.MapFS {
	return fstest.MapFS{
		"welcome.subject.tmpl": {Data: []byte("Welcome, {{.Name}}\n")},
		"welcome.text.tmpl":    {Data: []byte("Hi {{.Name}}, open {{.URL}}\n")},
		"welcome.html.gohtml":  {Data: []byte(`<p>Hi {{.Name}}</p><a href="{{.URL}}">open</a><script>var n = {{.Name}};</script>`)},
	}
}

func TestTemplateRendersWithContextualEscaping(t *testing.T) {
	t.Parallel()
	tmpl, err := forgemail.NewTemplate[welcomeData](welcomeFS(), "welcome")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := tmpl.Render(welcomeData{Name: `<img src=x onerror=alert(1)> & "Zoë"`, URL: "javascript:alert(1)"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered.HTML, "<img") || strings.Contains(rendered.HTML, `onerror=alert`) && !strings.Contains(rendered.HTML, "&lt;img") {
		t.Fatalf("HTML not escaped: %s", rendered.HTML)
	}
	if !strings.Contains(rendered.HTML, "&lt;img src=x onerror=alert(1)&gt; &amp; &#34;Zoë&#34;") {
		t.Fatalf("element text not escaped: %s", rendered.HTML)
	}
	if strings.Contains(rendered.HTML, `href="javascript:`) || !strings.Contains(rendered.HTML, "#ZgotmplZ") {
		t.Fatalf("dangerous URL not neutralised: %s", rendered.HTML)
	}
	if strings.Contains(rendered.HTML, `var n = <img`) || !strings.Contains(rendered.HTML, "var n = \"\\u003cimg") {
		t.Fatalf("script context not escaped as JS string: %s", rendered.HTML)
	}
	if rendered.Text != "Hi <img src=x onerror=alert(1)> & \"Zoë\", open javascript:alert(1)\n" {
		t.Fatalf("text part must not be HTML-escaped: %q", rendered.Text)
	}
	if rendered.Subject != `Welcome, <img src=x onerror=alert(1)> & "Zoë"` {
		t.Fatalf("subject = %q", rendered.Subject)
	}
}

func TestTemplateMessageAndDeliver(t *testing.T) {
	t.Parallel()
	tmpl, err := forgemail.NewTemplate[welcomeData](welcomeFS(), "welcome")
	if err != nil {
		t.Fatal(err)
	}
	recorder := mailtest.NewRecorder()
	base := baseMessage()
	base.Subject = ""
	base.Text = ""
	if err := tmpl.Deliver(context.Background(), recorder, base, welcomeData{Name: "Alice", URL: "https://example.com/start"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	delivery, _ := recorder.Last()
	if delivery.Message.Subject != "Welcome, Alice" || !strings.Contains(delivery.Message.HTML, `href="https://example.com/start"`) || !strings.HasPrefix(delivery.Message.Text, "Hi Alice") {
		t.Fatalf("message = %+v", delivery.Message)
	}
	parsed, err := delivery.Parse()
	if err != nil || !strings.HasPrefix(parsed.Header.Get("Content-Type"), "multipart/alternative") {
		t.Fatalf("not multipart/alternative: %v %v", parsed.Header.Get("Content-Type"), err)
	}
	if base.Text != "" || base.Subject != "" {
		t.Fatal("base message was mutated")
	}

	// An explicit subject on the base message wins over the template's.
	base.Subject = "Custom"
	if msg, err := tmpl.Message(base, welcomeData{Name: "A", URL: "https://example.com"}); err != nil || msg.Subject != "Custom" {
		t.Fatalf("Message = %+v, %v", msg, err)
	}
}

func TestTemplateFailureSendsNothing(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"broken.text.tmpl":   {Data: []byte("fine so far {{.Missing.Field}}")},
		"broken.html.gohtml": {Data: []byte("<p>html</p>")},
	}
	tmpl, err := forgemail.NewTemplate[welcomeData](fsys, "broken")
	if err != nil {
		t.Fatal(err)
	}
	recorder := mailtest.NewRecorder()
	err = tmpl.Deliver(context.Background(), recorder, baseMessage(), welcomeData{})
	if !errors.Is(err, forgemail.ErrTemplate) {
		t.Fatalf("err = %v, want ErrTemplate", err)
	}
	if recorder.Len() != 0 {
		t.Fatal("a message was sent although the template failed")
	}

	// The HTML part failing after the text part succeeded must not send the
	// text part either.
	fsys = fstest.MapFS{
		"half.text.tmpl":   {Data: []byte("ok")},
		"half.html.gohtml": {Data: []byte("<p>{{.Nope.Deeper}}</p>")},
	}
	half, err := forgemail.NewTemplate[welcomeData](fsys, "half")
	if err != nil {
		t.Fatal(err)
	}
	if err := half.Deliver(context.Background(), recorder, baseMessage(), welcomeData{}); !errors.Is(err, forgemail.ErrTemplate) || recorder.Len() != 0 {
		t.Fatalf("err = %v, deliveries = %d", err, recorder.Len())
	}
}

func TestTemplateDataCannotInjectHeadersThroughSubject(t *testing.T) {
	t.Parallel()
	tmpl, err := forgemail.NewTemplate[welcomeData](welcomeFS(), "welcome")
	if err != nil {
		t.Fatal(err)
	}
	recorder := mailtest.NewRecorder()
	base := baseMessage()
	base.Subject = ""
	err = tmpl.Deliver(context.Background(), recorder, base, welcomeData{Name: "x\r\nBcc: evil@example.net", URL: "https://example.com"})
	if !errors.Is(err, forgemail.ErrInvalid) || recorder.Len() != 0 {
		t.Fatalf("err = %v, deliveries = %d", err, recorder.Len())
	}
}

func TestNewTemplateErrors(t *testing.T) {
	t.Parallel()
	for name, fsys := range map[string]fstest.MapFS{
		"missing files":      {},
		"subject only":       {"x.subject.tmpl": {Data: []byte("s")}},
		"text syntax error":  {"x.text.tmpl": {Data: []byte("{{.Name")}},
		"html syntax error":  {"x.html.gohtml": {Data: []byte("{{if}}")}},
		"html context error": {"x.html.gohtml": {Data: []byte(`<a href="{{.U}}`)}},
	} {
		if _, err := forgemail.NewTemplate[welcomeData](fsys, "x"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := forgemail.NewTemplate[welcomeData](nil, "x"); err == nil {
		t.Error("nil fs accepted")
	}
	for _, name := range []string{"", ".", "../x", "/abs", "a//b"} {
		if _, err := forgemail.NewTemplate[welcomeData](welcomeFS(), name); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
}

func TestTemplateOnlyTextOrOnlyHTML(t *testing.T) {
	t.Parallel()
	textOnly, err := forgemail.NewTemplate[welcomeData](fstest.MapFS{"t.text.tmpl": {Data: []byte("text {{.Name}}")}}, "t")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := textOnly.Message(baseMessage(), welcomeData{Name: "A"})
	if err != nil || msg.HTML != "" || msg.Text != "text A" || msg.Subject != "Welcome" {
		t.Fatalf("text-only message = %+v, %v", msg, err)
	}
	htmlOnly, err := forgemail.NewTemplate[welcomeData](fstest.MapFS{"h.html.gohtml": {Data: []byte("<b>{{.Name}}</b>")}}, "h")
	if err != nil {
		t.Fatal(err)
	}
	msg, err = htmlOnly.Message(baseMessage(), welcomeData{Name: "<A>"})
	if err != nil || msg.HTML != "<b>&lt;A&gt;</b>" {
		t.Fatalf("html-only message = %+v, %v", msg, err)
	}
	if msg.Text != baseMessage().Text {
		t.Fatalf("base text lost: %q", msg.Text)
	}
}

func TestTemplatePartialsAndFuncs(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"mail.html.gohtml":            {Data: []byte(`{{template "header" .}}<p>{{shout .Name}}</p>{{template "footer"}}`)},
		"mail.text.tmpl":              {Data: []byte(`{{template "sig"}}{{shout .Name}}`)},
		"partials/header.html.gohtml": {Data: []byte(`{{define "header"}}<h1>{{.Name}}</h1>{{end}}`)},
		"partials/footer.html.gohtml": {Data: []byte(`{{define "footer"}}<footer>&copy; Forge</footer>{{end}}`)},
		"partials/sig.text.tmpl":      {Data: []byte("{{define \"sig\"}}-- Forge\n{{end}}")},
	}
	tmpl, err := forgemail.NewTemplate[welcomeData](fsys, "mail",
		forgemail.WithFuncs(map[string]any{"shout": strings.ToUpper}),
		forgemail.WithHTMLPartials("partials/*.html.gohtml"),
		forgemail.WithTextPartials("partials/*.text.tmpl"),
	)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := tmpl.Render(welcomeData{Name: "alice <b>"})
	if err != nil {
		t.Fatal(err)
	}
	if rendered.HTML != "<h1>alice &lt;b&gt;</h1><p>ALICE &lt;B&gt;</p><footer>&copy; Forge</footer>" {
		t.Fatalf("html = %q", rendered.HTML)
	}
	if rendered.Text != "-- Forge\nALICE <B>" {
		t.Fatalf("text = %q", rendered.Text)
	}
	if _, err := forgemail.NewTemplate[welcomeData](fsys, "mail", forgemail.WithFuncs(map[string]any{"shout": strings.ToUpper}), forgemail.WithHTMLPartials("nothing/*.gohtml")); err == nil {
		t.Fatal("partial pattern matching nothing accepted")
	}
}

func TestTemplateIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	tmpl, err := forgemail.NewTemplate[welcomeData](welcomeFS(), "welcome")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() {
			_, err := tmpl.Render(welcomeData{Name: "A", URL: "https://example.com"})
			done <- err
		}()
	}
	for i := 0; i < 16; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
