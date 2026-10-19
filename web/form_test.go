package web_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

type postForm struct {
	Title       string     `form:"title"`
	Published   bool       `form:"published"`
	Featured    *bool      `form:"featured"`
	Rank        int        `form:"rank"`
	Views       uint16     `form:"views"`
	Limit       *int64     `form:"limit"`
	PublishedAt time.Time  `form:"published_at"`
	RetiredAt   *time.Time `form:"retired_at"`
	AuthorID    uuid.UUID  `form:"author_id"`
	ParentID    *uuid.UUID `form:"parent_id"`
	Summary     *string    `form:"summary"`
	Tags        []string   `form:"tags"`
	Internal    bool       // untagged: never bound
	Skipped     string     `form:"-"`
}

func formRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/posts", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request
}

func TestDecodeFormBindsSupportedTypes(t *testing.T) {
	t.Parallel()
	author := uuid.MustNew()
	values := url.Values{
		"title":              {"Hello <world>"},
		"published":          {"on"},
		"featured":           {"false"},
		"rank":               {"-3"},
		"views":              {"65535"},
		"limit":              {"9000000000"},
		"published_at":       {"2026-10-19T08:30"},
		"retired_at":         {"2026-10-19T08:30:00-03:00"},
		"author_id":          {author.String()},
		"parent_id":          {""},
		"summary":            {""},
		"tags":               {"go", "html"},
		"_method":            {"patch"},
		"authenticity_token": {"ignored"},
	}
	form := postForm{Internal: true, Skipped: "kept"}
	if err := web.DecodeForm(formRequest(values.Encode()), &form); err != nil {
		t.Fatal(err)
	}
	if form.Title != "Hello <world>" || !form.Published || form.Featured == nil || *form.Featured || form.Rank != -3 || form.Views != 65535 {
		t.Fatalf("scalars = %+v", form)
	}
	if form.Limit == nil || *form.Limit != 9_000_000_000 {
		t.Fatalf("limit = %v", form.Limit)
	}
	if want := time.Date(2026, 10, 19, 8, 30, 0, 0, time.UTC); !form.PublishedAt.Equal(want) || form.PublishedAt.Location() != time.UTC {
		t.Fatalf("published_at = %v", form.PublishedAt)
	}
	if form.RetiredAt == nil || !form.RetiredAt.Equal(time.Date(2026, 10, 19, 11, 30, 0, 0, time.UTC)) {
		t.Fatalf("retired_at = %v", form.RetiredAt)
	}
	if form.AuthorID != author || form.ParentID != nil || form.Summary != nil {
		t.Fatalf("ids/optional = %+v", form)
	}
	if len(form.Tags) != 2 || form.Tags[0] != "go" || form.Tags[1] != "html" {
		t.Fatalf("tags = %v", form.Tags)
	}
	if !form.Internal || form.Skipped != "kept" {
		t.Fatalf("untagged fields were touched: %+v", form)
	}
}

func TestDecodeFormCheckboxAbsentIsFalseAndOptionalAbsentIsNil(t *testing.T) {
	t.Parallel()
	featured := true
	form := postForm{Published: true, Featured: &featured, Rank: 1, PublishedAt: time.Now()}
	err := web.DecodeForm(formRequest("title=x&rank=2&views=1&published_at=2026-01-02&author_id="+uuid.MustNew().String()), &form)
	if err != nil {
		t.Fatal(err)
	}
	if form.Published || form.Featured != nil || form.Rank != 2 || form.Tags != nil {
		t.Fatalf("form = %+v", form)
	}
}

func TestDecodeFormReportsFieldErrors(t *testing.T) {
	t.Parallel()
	var form postForm
	err := web.DecodeForm(formRequest("title=ok&published=maybe&rank=abc&views=70000&published_at=yesterday&author_id=not-a-uuid&limit=1.5"), &form)
	var problems web.FormErrors
	if !errors.As(err, &problems) {
		t.Fatalf("error = %v, want FormErrors", err)
	}
	want := map[string]string{
		"published":    "invalid",
		"rank":         "invalid",
		"views":        "out_of_range",
		"published_at": "invalid",
		"author_id":    "invalid",
		"limit":        "invalid",
	}
	if len(problems) != len(want) {
		t.Fatalf("problems = %+v", problems)
	}
	for _, problem := range problems {
		if want[problem.Field] != problem.Code || problem.Message == "" {
			t.Fatalf("problem = %+v", problem)
		}
		if strings.Contains(problem.Message, "abc") || strings.Contains(problem.Message, "yesterday") {
			t.Fatalf("message echoes input: %+v", problem)
		}
	}
	if !problems.Has("rank") || problems.Message("rank") != "must be a whole number" || problems.Has("title") || len(problems.Messages("views")) != 1 {
		t.Fatalf("accessors: %+v", problems)
	}
	if public := fault.From(err); public == nil || public.Status() != http.StatusUnprocessableEntity || public.Code != "invalid_fields" {
		t.Fatalf("fault = %+v", public)
	}
}

func TestDecodeFormRequiresNonPointerValues(t *testing.T) {
	t.Parallel()
	var form postForm
	err := web.DecodeForm(formRequest("title=x"), &form)
	var problems web.FormErrors
	if !errors.As(err, &problems) {
		t.Fatalf("error = %v", err)
	}
	for _, field := range []string{"rank", "views", "published_at", "author_id"} {
		if !problems.Has(field) {
			t.Fatalf("%s: missing blank error in %+v", field, problems)
		}
	}
	if problems.Has("limit") || problems.Has("retired_at") || problems.Has("published") {
		t.Fatalf("optional fields reported: %+v", problems)
	}
}

func TestDecodeFormRejectsMalformedRequests(t *testing.T) {
	t.Parallel()
	var formsWithBody = func(body string) *http.Request { return formRequest(body) }
	tests := []struct {
		name    string
		request *http.Request
		status  int
		code    string
	}{
		{name: "unknown field", request: formsWithBody("title=x&admin=true"), status: 400, code: "invalid_form"},
		{name: "untagged field", request: formsWithBody("title=x&Internal=true"), status: 400, code: "invalid_form"},
		{name: "long unknown field", request: formsWithBody(strings.Repeat("x", 100) + "=1"), status: 400, code: "invalid_form"},
		{name: "duplicate scalar", request: formsWithBody("title=a&title=b"), status: 400, code: "invalid_form"},
		{name: "duplicate checkbox", request: formsWithBody("published=0&published=1"), status: 400, code: "invalid_form"},
		{name: "semicolon separator", request: formsWithBody("title=a;rank=1"), status: 400, code: "invalid_form"},
		{name: "bad escape", request: formsWithBody("title=%zz"), status: 400, code: "invalid_form"},
		{name: "too many fields", request: formsWithBody(strings.Repeat("tags=a&", web.MaxFormFields) + "tags=a"), status: 400, code: "invalid_form"},
		{name: "too large", request: formsWithBody("title=" + strings.Repeat("a", web.MaxFormBytes)), status: 413, code: "body_too_large"},
		{name: "json body", request: func() *http.Request {
			request := httptest.NewRequest(http.MethodPost, "/posts", strings.NewReader(`{"title":"x"}`))
			request.Header.Set("Content-Type", "application/json")
			return request
		}(), status: 415, code: "unsupported_media_type"},
		{name: "multipart body", request: func() *http.Request {
			request := httptest.NewRequest(http.MethodPost, "/posts", strings.NewReader(""))
			request.Header.Set("Content-Type", "multipart/form-data; boundary=x")
			return request
		}(), status: 415, code: "unsupported_media_type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var form postForm
			err := web.DecodeForm(tt.request, &form)
			public := fault.From(err)
			if public == nil || public.Status() != tt.status || public.Code != tt.code {
				t.Fatalf("error = %v, want %d %s", err, tt.status, tt.code)
			}
		})
	}
}

func TestDecodeFormDoesNotReuseAnotherBodyParse(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/posts", strings.NewReader(`{"title":"x"}`))
	request.Header.Set("Content-Type", "application/json")
	_ = request.ParseForm() // leaves an empty PostForm behind
	var form postForm
	if public := fault.From(web.DecodeForm(request, &form)); public == nil || public.Status() != http.StatusUnsupportedMediaType {
		t.Fatalf("fault = %+v", public)
	}
	long := formRequest(strings.Repeat("x", 100) + "=1")
	if err := web.DecodeForm(long, &form); err == nil || strings.Contains(err.Error(), strings.Repeat("x", 100)) {
		t.Fatalf("error echoes a long field name: %v", err)
	}
}

func TestDecodeFormRespectsServerBodyLimit(t *testing.T) {
	t.Parallel()
	request := formRequest("title=" + strings.Repeat("a", 100))
	request.Body = http.MaxBytesReader(httptest.NewRecorder(), request.Body, 16)
	var form postForm
	if public := fault.From(web.DecodeForm(request, &form)); public == nil || public.Status() != http.StatusRequestEntityTooLarge {
		t.Fatalf("fault = %+v", public)
	}
}

func TestDecodeFormIgnoresQueryString(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/posts?title=query&admin=true", strings.NewReader("title=body"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var form struct {
		Title string `form:"title"`
	}
	if err := web.DecodeForm(request, &form); err != nil || form.Title != "body" {
		t.Fatalf("title=%q err=%v", form.Title, err)
	}
	// The parsed body is cached for later readers.
	if request.PostForm.Get("title") != "body" || request.FormValue("title") != "body" {
		t.Fatalf("PostForm=%v", request.PostForm)
	}
}

func TestDecodeFormRejectsInvalidDestinations(t *testing.T) {
	t.Parallel()
	type reserved struct {
		Token string `form:"authenticity_token"`
	}
	type duplicate struct {
		A string `form:"a"`
		B string `form:"a"`
	}
	type unsupported struct {
		Ratio float64 `form:"ratio"`
	}
	type unexported struct {
		hidden string `form:"hidden"`
	}
	var name string
	for _, destination := range []any{nil, name, &name, &reserved{}, &duplicate{}, &unsupported{}, &unexported{}} {
		err := web.DecodeForm(formRequest(""), destination)
		if err == nil || fault.From(err) != nil {
			t.Fatalf("%T: error = %v, want a programming error", destination, err)
		}
	}
	_ = unexported{}.hidden
}

func TestFormErrorsAdd(t *testing.T) {
	t.Parallel()
	var problems web.FormErrors
	problems.Add("title", "taken", "is already taken")
	if !problems.Has("title") || problems.Error() != "invalid fields: title is already taken" || !errors.Is(problems, web.ErrInvalidFields) {
		t.Fatalf("problems = %+v (%v)", problems, problems)
	}
}
