package turbo_test

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/turbo"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestStreamElements(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		build func() (turbo.Stream, error)
		want  string
	}{
		{func() (turbo.Stream, error) { return turbo.Append("posts", "<li>1</li>") }, `<turbo-stream action="append" target="posts"><template><li>1</li></template></turbo-stream>` + "\n"},
		{func() (turbo.Stream, error) { return turbo.Prepend("posts", "<li>0</li>") }, `<turbo-stream action="prepend" target="posts"><template><li>0</li></template></turbo-stream>` + "\n"},
		{func() (turbo.Stream, error) { return turbo.Replace("post_1", "<li>x</li>") }, `<turbo-stream action="replace" target="post_1"><template><li>x</li></template></turbo-stream>` + "\n"},
		{func() (turbo.Stream, error) { return turbo.Update("flash", "Saved") }, `<turbo-stream action="update" target="flash"><template>Saved</template></turbo-stream>` + "\n"},
		{func() (turbo.Stream, error) { return turbo.Update("flash", "") }, `<turbo-stream action="update" target="flash"><template></template></turbo-stream>` + "\n"},
		{func() (turbo.Stream, error) { return turbo.Remove("post_1") }, `<turbo-stream action="remove" target="post_1"></turbo-stream>` + "\n"},
		{func() (turbo.Stream, error) { return turbo.Before("post_2", "<hr>") }, `<turbo-stream action="before" target="post_2"><template><hr></template></turbo-stream>` + "\n"},
		{func() (turbo.Stream, error) { return turbo.After("post_2", "<hr>") }, `<turbo-stream action="after" target="post_2"><template><hr></template></turbo-stream>` + "\n"},
	} {
		stream, err := test.build()
		if err != nil {
			t.Fatal(err)
		}
		if got := string(stream.HTML()); got != test.want {
			t.Errorf("HTML =\n%s\nwant\n%s", got, test.want)
		}
	}
}

func TestTargetsCannotInjectMarkup(t *testing.T) {
	t.Parallel()
	for _, target := range []string{
		"", "1post", "-post", "_post", "a b", `a"b`, `a" onload="x`, "a>b", "a<b", "a&b", "a'b", "a\nb", "é", "a.b", "a/b", "a#b",
		strings.Repeat("a", turbo.MaxTargetBytes+1),
	} {
		if _, err := turbo.Append(target, "x"); err == nil {
			t.Errorf("target %q accepted", target)
		}
		if _, err := turbo.Remove(target); err == nil {
			t.Errorf("remove target %q accepted", target)
		}
	}
	for _, target := range []string{"a", "post_1", "post:1", "a-b", "A9", strings.Repeat("a", turbo.MaxTargetBytes)} {
		if _, err := turbo.Append(target, "x"); err != nil {
			t.Errorf("target %q rejected: %v", target, err)
		}
	}
}

func TestActionRules(t *testing.T) {
	t.Parallel()
	if _, err := turbo.New(turbo.ActionRemove, "a", "<b>"); err == nil {
		t.Error("remove with content accepted")
	}
	if _, err := turbo.New("refresh", "a", ""); err == nil {
		t.Error("unknown action accepted")
	}
	if _, err := turbo.New("", "a", ""); err == nil {
		t.Error("empty action accepted")
	}
}

// Content comes from html/template, so user input is already escaped when it
// reaches a stream; the stream must not escape or alter it again.
func TestContentFromTemplatesStaysEscaped(t *testing.T) {
	t.Parallel()
	page := template.Must(template.New("row").Parse(`<tr id="post_1"><td>{{.}}</td></tr>`))
	var body strings.Builder
	if err := page.Execute(&body, `</template><script>alert(1)</script>`); err != nil {
		t.Fatal(err)
	}
	stream, err := turbo.Replace("post_1", template.HTML(body.String())) // #nosec G203 -- html/template output
	if err != nil {
		t.Fatal(err)
	}
	html := string(stream.HTML())
	if strings.Contains(html, "<script>") || strings.Count(html, "</template>") != 1 {
		t.Fatalf("user input escaped the template element: %s", html)
	}
	if !strings.Contains(html, "&lt;/template&gt;&lt;script&gt;") {
		t.Fatalf("input was not escaped: %s", html)
	}
}

func TestDomID(t *testing.T) {
	t.Parallel()
	id := uuid.MustNew()
	for _, test := range []struct {
		prefix string
		id     any
		want   string
	}{
		{"post", id, "post_" + id.String()},
		{"post", uuid.UUID{}, "new_post"},
		{"edit_post", 7, "edit_post_7"},
		{"post", "a-1", "post_a-1"},
	} {
		got, err := turbo.DomID(test.prefix, test.id)
		if err != nil || got != test.want {
			t.Errorf("DomID(%q, %v) = %q, %v; want %q", test.prefix, test.id, got, err, test.want)
		}
		if err := turbo.ValidTarget(got); err != nil {
			t.Errorf("DomID produced an invalid target %q", got)
		}
	}
	for _, bad := range []struct {
		prefix string
		id     any
	}{{"Post", 1}, {"", 1}, {"1post", 1}, {"post", "a b"}, {"post", `"><script>`}, {"post", 1.5}, {"post", ""}} {
		if got, err := turbo.DomID(bad.prefix, bad.id); err == nil {
			t.Errorf("DomID(%q, %v) = %q, want an error", bad.prefix, bad.id, got)
		}
	}
}

func TestRequested(t *testing.T) {
	t.Parallel()
	for accept, want := range map[string]bool{
		"text/vnd.turbo-stream.html, text/html, application/xhtml+xml": true, // what Turbo sends with a form
		"text/vnd.turbo-stream.html":                                   true,
		"TEXT/VND.TURBO-STREAM.HTML;q=0.5, text/html":                  true,
		"text/html, application/xhtml+xml":                             false, // Turbo Drive navigation
		"*/*":                                                          false,
		"text/*":                                                       false,
		"application/json":                                             false,
		"text/vnd.turbo-stream.html;q=0":                               false,
		"":                                                             false,
	} {
		request := httptest.NewRequest(http.MethodPost, "/", nil)
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		if got := turbo.Requested(request); got != want {
			t.Errorf("Requested(%q) = %v, want %v", accept, got, want)
		}
	}
}

func TestRender(t *testing.T) {
	t.Parallel()
	first, _ := turbo.Remove("post_1")
	second, _ := turbo.Update("flash", "Deleted")
	recorder := httptest.NewRecorder()
	if err := turbo.Render(recorder, http.StatusOK, first, second); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/vnd.turbo-stream.html; charset=utf-8" || recorder.Header().Get("Vary") != "Accept" {
		t.Fatalf("status=%d headers=%v", recorder.Code, recorder.Header())
	}
	want := `<turbo-stream action="remove" target="post_1"></turbo-stream>` + "\n" +
		`<turbo-stream action="update" target="flash"><template>Deleted</template></turbo-stream>` + "\n"
	if recorder.Body.String() != want {
		t.Fatalf("body = %q", recorder.Body.String())
	}

	untouched := httptest.NewRecorder()
	if err := turbo.Render(untouched, http.StatusOK); err == nil || untouched.Body.Len() != 0 || untouched.Header().Get("Content-Type") != "" {
		t.Fatalf("empty render: err=%v body=%q", err, untouched.Body.String())
	}
	if err := turbo.Render(httptest.NewRecorder(), 42, first); err == nil {
		t.Fatal("invalid status accepted")
	}
	invalid := httptest.NewRecorder()
	if err := turbo.Render(invalid, http.StatusUnprocessableEntity, second); err != nil || invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("422 render: %v %d", err, invalid.Code)
	}
}

func TestJoinPreservesOrder(t *testing.T) {
	t.Parallel()
	a, _ := turbo.Append("a", "1")
	b, _ := turbo.Append("b", "2")
	if got := string(turbo.Join(a, b)); !strings.HasPrefix(got, `<turbo-stream action="append" target="a">`) || strings.Count(got, "<turbo-stream ") != 2 {
		t.Fatalf("Join = %q", got)
	}
	if turbo.Join() != "" {
		t.Fatal("empty Join is not empty")
	}
}
