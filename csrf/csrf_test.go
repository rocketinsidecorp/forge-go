package csrf_test

import (
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/web"
)

const site = "https://app.example.com"

func newToken(t *testing.T) string {
	t.Helper()
	token, err := csrf.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// protected returns the middleware around a handler that records the method
// it saw and echoes the request's token.
func protected(t *testing.T, opts csrf.Options) (http.Handler, *int) {
	t.Helper()
	protection, err := csrf.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	return protection.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		token, err := csrf.Token(w, r)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("X-Seen", r.Method+" "+token)
		w.WriteHeader(http.StatusNoContent)
	})), &calls
}

func formPost(target, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, site+target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request
}

func csrfCookie(response *http.Response) *http.Cookie {
	for _, cookie := range response.Cookies() {
		if cookie.Name == csrf.CookieName {
			return cookie
		}
	}
	return nil
}

func TestSafeRequestIssuesTokenOnlyWhenUsed(t *testing.T) {
	t.Parallel()
	protection, err := csrf.New(csrf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var first, second string
	handler := protection.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/form" {
			return
		}
		first, _ = csrf.Token(w, r)
		second, _ = csrf.Token(w, r)
	}))

	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, site+"/assets/app.css", nil))
	if asset.Header().Get("Set-Cookie") != "" {
		t.Fatalf("a response without a token set a cookie: %v", asset.Header())
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, site+"/form", nil))
	cookie := csrfCookie(page.Result())
	if cookie == nil || cookie.Value != first || first != second || !csrf.Match(first, first) {
		t.Fatalf("cookie=%+v first=%q second=%q", cookie, first, second)
	}
	if !cookie.Secure || cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("cookie attributes = %+v", cookie)
	}
	if len(page.Header().Values("Set-Cookie")) != 1 || page.Header().Get("Cache-Control") != "private" {
		t.Fatalf("headers = %v", page.Header())
	}

	// A valid cookie is reused, not rotated.
	again := httptest.NewRequest(http.MethodGet, site+"/form", nil)
	again.AddCookie(cookie)
	reused := httptest.NewRecorder()
	handler.ServeHTTP(reused, again)
	if first != cookie.Value || reused.Header().Get("Set-Cookie") != "" {
		t.Fatalf("token rotated: %q vs %q, %v", first, cookie.Value, reused.Header())
	}

	// A malformed cookie is replaced.
	malformed := httptest.NewRequest(http.MethodGet, site+"/form", nil)
	malformed.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: "short"})
	replaced := httptest.NewRecorder()
	handler.ServeHTTP(replaced, malformed)
	if fresh := csrfCookie(replaced.Result()); fresh == nil || fresh.Value == "short" || fresh.Value != first {
		t.Fatalf("malformed cookie not replaced: %v (token %q)", replaced.Header(), first)
	}
}

func TestUnsafeRequestsWithValidTokenPass(t *testing.T) {
	t.Parallel()
	token := newToken(t)
	handler, calls := protected(t, csrf.Options{})
	tests := []struct {
		name    string
		request func() *http.Request
	}{
		{name: "form field", request: func() *http.Request {
			return formPost("/posts", url.Values{"title": {"x"}, csrf.FormField: {token}}.Encode())
		}},
		{name: "header", request: func() *http.Request {
			request := httptest.NewRequest(http.MethodDelete, site+"/posts/1", nil)
			request.Header.Set(csrf.HeaderName, token)
			return request
		}},
		{name: "same-origin browser", request: func() *http.Request {
			request := formPost("/posts", csrf.FormField+"="+token)
			request.Header.Set("Sec-Fetch-Site", "same-origin")
			request.Header.Set("Origin", site)
			return request
		}},
	}
	for _, tt := range tests {
		request := tt.request()
		request.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
		recorder := httptest.NewRecorder()
		before := *calls
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent || *calls != before+1 || recorder.Header().Get("X-Seen") != request.Method+" "+token {
			t.Fatalf("%s: status=%d seen=%q body=%s", tt.name, recorder.Code, recorder.Header().Get("X-Seen"), recorder.Body.String())
		}
		if recorder.Header().Get("Set-Cookie") != "" {
			t.Fatalf("%s: token rotated on a valid request", tt.name)
		}
	}
}

func TestUnsafeRequestsAreRejected(t *testing.T) {
	t.Parallel()
	token := newToken(t)
	other := newToken(t)
	withCookie := func(request *http.Request, value string) *http.Request {
		request.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: value})
		return request
	}
	multipartBody := func() *http.Request {
		var body strings.Builder
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField(csrf.FormField, token)
		_ = writer.Close()
		request := httptest.NewRequest(http.MethodPost, site+"/posts", strings.NewReader(body.String()))
		request.Header.Set("Content-Type", writer.FormDataContentType())
		return request
	}
	tests := []struct {
		name    string
		request *http.Request
		status  int
		code    string
	}{
		{name: "missing token", request: withCookie(formPost("/posts", "title=x"), token), status: 403, code: "csrf_invalid"},
		{name: "missing cookie", request: formPost("/posts", csrf.FormField+"="+token), status: 403, code: "csrf_invalid"},
		{name: "wrong token", request: withCookie(formPost("/posts", csrf.FormField+"="+other), token), status: 403, code: "csrf_invalid"},
		{name: "token in query string ignored", request: withCookie(formPost("/posts?"+csrf.FormField+"="+token, "title=x"), token), status: 403, code: "csrf_invalid"},
		{name: "token in query string of bodiless delete", request: withCookie(httptest.NewRequest(http.MethodDelete, site+"/posts/1?"+csrf.FormField+"="+token, nil), token), status: 403, code: "csrf_invalid"},
		{name: "malformed cookie", request: withCookie(formPost("/posts", csrf.FormField+"=abc"), "abc"), status: 403, code: "csrf_invalid"},
		{name: "padded cookie", request: withCookie(formPost("/posts", csrf.FormField+"="+token+"="), token+"="), status: 403, code: "csrf_invalid"},
		{name: "two cookies", request: withCookie(withCookie(formPost("/posts", csrf.FormField+"="+token), token), token), status: 403, code: "csrf_invalid"},
		{name: "repeated form field", request: withCookie(formPost("/posts", csrf.FormField+"="+token+"&"+csrf.FormField+"="+token), token), status: 403, code: "csrf_invalid"},
		{name: "invalid header does not fall back to form", request: func() *http.Request {
			request := withCookie(formPost("/posts", csrf.FormField+"="+token), token)
			request.Header.Set(csrf.HeaderName, other)
			return request
		}(), status: 403, code: "csrf_invalid"},
		{name: "repeated header", request: func() *http.Request {
			request := withCookie(httptest.NewRequest(http.MethodPost, site+"/posts", nil), token)
			request.Header.Add(csrf.HeaderName, token)
			request.Header.Add(csrf.HeaderName, token)
			return request
		}(), status: 403, code: "csrf_invalid"},
		{name: "multipart field is not read", request: withCookie(multipartBody(), token), status: 403, code: "csrf_invalid"},
		{name: "text/plain body", request: func() *http.Request {
			request := withCookie(httptest.NewRequest(http.MethodPost, site+"/posts", strings.NewReader(csrf.FormField+"="+token)), token)
			request.Header.Set("Content-Type", "text/plain")
			return request
		}(), status: 403, code: "csrf_invalid"},
		{name: "basic authorization is ambient", request: func() *http.Request {
			request := withCookie(formPost("/posts", "title=x"), token)
			request.SetBasicAuth("user", "password")
			return request
		}(), status: 403, code: "csrf_invalid"},
		{name: "cross-site origin with valid token", request: func() *http.Request {
			request := withCookie(formPost("/posts", csrf.FormField+"="+token), token)
			request.Header.Set("Origin", "https://evil.example")
			return request
		}(), status: 403, code: "cross_origin_request"},
		{name: "cross-site fetch metadata", request: func() *http.Request {
			request := withCookie(formPost("/posts", csrf.FormField+"="+token), token)
			request.Header.Set("Sec-Fetch-Site", "cross-site")
			return request
		}(), status: 403, code: "cross_origin_request"},
		{name: "same-site sibling subdomain", request: func() *http.Request {
			request := withCookie(formPost("/posts", csrf.FormField+"="+token), token)
			request.Header.Set("Sec-Fetch-Site", "same-site")
			request.Header.Set("Origin", "https://blog.example.com")
			return request
		}(), status: 403, code: "cross_origin_request"},
		{name: "cross-site JSON", request: func() *http.Request {
			request := httptest.NewRequest(http.MethodPost, site+"/api", strings.NewReader("{}"))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Sec-Fetch-Site", "cross-site")
			return request
		}(), status: 403, code: "cross_origin_request"},
		{name: "oversized body", request: withCookie(formPost("/posts", "title="+strings.Repeat("a", web.MaxFormBytes)+"&"+csrf.FormField+"="+token), token), status: 413, code: "body_too_large"},
		{name: "malformed body", request: withCookie(formPost("/posts", "title=%zz&"+csrf.FormField+"="+token), token), status: 400, code: "invalid_form"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handler, calls := protected(t, csrf.Options{})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, tt.request)
			if recorder.Code != tt.status || *calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", recorder.Code, *calls, recorder.Body.String())
			}
			var body web.ErrorBody
			if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil || body.Error.Code != tt.code {
				t.Fatalf("body=%+v err=%v", body, err)
			}
			if recorder.Header().Get("Set-Cookie") != "" {
				t.Fatalf("a rejection issued a cookie: %v", recorder.Header())
			}
		})
	}
}

func TestRequestsCrossSitePagesCannotForgeSkipTheToken(t *testing.T) {
	t.Parallel()
	handler, calls := protected(t, csrf.Options{})
	bearer := httptest.NewRequest(http.MethodDelete, site+"/v1/posts/1", nil)
	bearer.Header.Set("Authorization", "Bearer opaque-token")
	jsonBody := httptest.NewRequest(http.MethodPost, site+"/auth/login", strings.NewReader("{}"))
	jsonBody.Header.Set("Content-Type", "application/json; charset=utf-8")
	problem := httptest.NewRequest(http.MethodPost, site+"/v1/posts", strings.NewReader("{}"))
	problem.Header.Set("Content-Type", "application/merge-patch+json")
	for _, request := range []*http.Request{bearer, jsonBody, problem} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("%s %s: status=%d body=%s", request.Method, request.URL, recorder.Code, recorder.Body.String())
		}
	}
	if *calls != 3 {
		t.Fatalf("calls = %d", *calls)
	}
}

func TestTrustedOriginsAndExemptPatterns(t *testing.T) {
	t.Parallel()
	token := newToken(t)
	handler, calls := protected(t, csrf.Options{
		TrustedOrigins: []string{"https://admin.example.com"},
		Exempt:         []string{"POST /webhooks/payments"},
	})
	trusted := formPost("/posts", csrf.FormField+"="+token)
	trusted.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
	trusted.Header.Set("Origin", "https://admin.example.com")
	trusted.Header.Set("Sec-Fetch-Site", "same-site")
	trustedWithoutToken := formPost("/posts", "title=x")
	trustedWithoutToken.Header.Set("Origin", "https://admin.example.com")
	webhook := formPost("/webhooks/payments", "event=paid")
	webhook.Header.Set("Origin", "https://payments.example")
	notExempt := formPost("/webhooks/other", "event=paid")

	for _, tt := range []struct {
		request *http.Request
		status  int
	}{{trusted, 204}, {trustedWithoutToken, 403}, {webhook, 204}, {notExempt, 403}} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, tt.request)
		if recorder.Code != tt.status {
			t.Fatalf("%s: status=%d body=%s", tt.request.URL, recorder.Code, recorder.Body.String())
		}
	}
	if *calls != 2 {
		t.Fatalf("calls = %d", *calls)
	}
}

// An exempt subtree pattern exempts only the paths under it: ServeMux answers
// "/hooks" with a redirect handler and the "/hooks/" pattern, which must not
// count as a match. Unclean paths that ServeMux would redirect are not exempt
// either.
func TestExemptPatternsMatchOnlyTheirRoutes(t *testing.T) {
	t.Parallel()
	handler, calls := protected(t, csrf.Options{Exempt: []string{"POST /hooks/"}})
	for _, tt := range []struct {
		target string
		status int
	}{{"/hooks/stripe", 204}, {"/hooks", 403}, {"/hooks/../posts", 403}, {"/hooks//stripe", 403}} {
		request := formPost(tt.target, "event=paid")
		request.Header.Set("Sec-Fetch-Site", "cross-site")
		request.Header.Set("Origin", "https://payments.example")
		if tt.status == 204 {
			request.Header.Del("Sec-Fetch-Site")
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != tt.status {
			t.Fatalf("%s: status=%d body=%s", tt.target, recorder.Code, recorder.Body.String())
		}
	}
	if *calls != 1 {
		t.Fatalf("calls = %d", *calls)
	}
}

// A method override never rides on an exemption, whichever order the
// middleware is installed in: an exempt POST that carries _method is
// verified like any other unsafe request.
func TestExemptPostCannotCarryMethodOverride(t *testing.T) {
	t.Parallel()
	protection, err := csrf.New(csrf.Options{Exempt: []string{"POST /hooks/{provider}"}})
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	handler := protection.Handler(web.MethodOverride(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method
		w.WriteHeader(http.StatusNoContent)
	})))
	forged := formPost("/hooks/stripe", "_method=delete")
	forged.Header.Set("Sec-Fetch-Site", "cross-site")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, forged)
	if recorder.Code != http.StatusForbidden || seen != "" {
		t.Fatalf("exempt POST with _method: status=%d seen=%q", recorder.Code, seen)
	}
	plain := formPost("/hooks/stripe", "event=paid")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, plain)
	if recorder.Code != http.StatusNoContent || seen != http.MethodPost {
		t.Fatalf("exempt POST: status=%d seen=%q", recorder.Code, seen)
	}
}

func TestRejectionPrefersHTMLForBrowsers(t *testing.T) {
	t.Parallel()
	handler, _ := protected(t, csrf.Options{})
	request := formPost("/posts", "title=<script>")
	request.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusForbidden || recorder.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		recorder.Header().Get("Cache-Control") != "no-store" || !strings.Contains(body, "<h1>Forbidden</h1>") || strings.Contains(body, "<script>") {
		t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), body)
	}
}

func TestMethodOverrideBehindProtection(t *testing.T) {
	t.Parallel()
	token := newToken(t)
	protection, err := csrf.New(csrf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	handler := web.MethodOverride(protection.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method
		w.WriteHeader(http.StatusNoContent)
	})))
	request := formPost("/posts/1", url.Values{"_method": {"DELETE"}, csrf.FormField: {token}}.Encode())
	request.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || seen != http.MethodDelete {
		t.Fatalf("status=%d seen=%q", recorder.Code, seen)
	}
	forged := formPost("/posts/1", "_method=DELETE")
	forged.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
	seen = ""
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, forged)
	if recorder.Code != http.StatusForbidden || seen != "" {
		t.Fatalf("overridden DELETE without token: status=%d seen=%q", recorder.Code, seen)
	}
}

// A token issued for HTML forms is accepted by auth.RequireCSRF, and a token
// issued at cookie login is accepted by the form middleware: both use the
// same cookie and format.
func TestInteroperatesWithAuthCookieTokens(t *testing.T) {
	t.Parallel()
	if auth.CSRFCookieName != csrf.CookieName || auth.CSRFHeaderName != csrf.HeaderName || !errors.Is(csrf.ErrInvalid, auth.ErrCSRFInvalid) {
		t.Fatal("auth and csrf disagree on the CSRF cookie, header or error")
	}
	protection, err := csrf.New(csrf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var issued string
	protection.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issued, _ = csrf.Token(w, r)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, site+"/", nil))

	required, err := auth.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, site+"/auth/cookie/logout", nil)
	request.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: issued})
	request.Header.Set(auth.CSRFHeaderName, issued)
	recorder := httptest.NewRecorder()
	protection.Handler(required).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestTokenRequiresMiddlewareAndOptionsAreValidated(t *testing.T) {
	t.Parallel()
	if _, err := csrf.Token(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, csrf.ErrNotInstalled) {
		t.Fatalf("Token without middleware: %v", err)
	}
	for _, opts := range []csrf.Options{
		{TrustedOrigins: []string{"not an origin"}},
		{TrustedOrigins: []string{"https://example.com/path"}},
		{Exempt: []string{"POST"}},
		{Exempt: []string{"POST /a", "POST /a"}},
	} {
		if _, err := csrf.New(opts); err == nil {
			t.Fatalf("New(%+v) succeeded", opts)
		}
	}
}

func TestParseTokenAcceptsOnlyCanonicalTokens(t *testing.T) {
	t.Parallel()
	token := newToken(t)
	if _, err := csrf.ParseToken(token); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", token + "=", token[:len(token)-1], strings.ToUpper(token[:1]) + token[1:] + "A", "+" + token[1:], token[:20] + "\n" + token[20:]} {
		if value == token {
			continue
		}
		if _, err := csrf.ParseToken(value); !errors.Is(err, csrf.ErrInvalid) {
			t.Fatalf("%q accepted", value)
		}
		if csrf.Match(token, value) || csrf.Match(value, value) && value != token {
			t.Fatalf("%q matched", value)
		}
	}
}
