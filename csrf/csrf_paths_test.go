package csrf_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/csrf"
)

func TestHandlerRefusesANilHandler(t *testing.T) {
	t.Parallel()
	protection, err := csrf.New(csrf.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if got := recover(); got != "csrf: nil handler" {
			t.Errorf("recovered %v, want the nil handler panic", got)
		}
	}()
	protection.Handler(nil)
	t.Error("Handler(nil) returned instead of panicking")
}

// An exempt route is exempt for any method it names, not only POST, and an
// exempt POST that is not a form has no method override to look for.
func TestExemptRoutesOfOtherMethodsAndBodiesSkipTheTokenCheck(t *testing.T) {
	t.Parallel()
	handler, calls := protected(t, csrf.Options{Exempt: []string{"PUT /hooks/{id}", "POST /events"}})
	put := httptest.NewRequest(http.MethodPut, site+"/hooks/1", strings.NewReader("event=paid"))
	put.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post := httptest.NewRequest(http.MethodPost, site+"/events", strings.NewReader("plain text"))
	post.Header.Set("Content-Type", "text/plain")
	for name, request := range map[string]*http.Request{"PUT": put, "non-form POST": post} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Errorf("%s: status=%d body=%s", name, recorder.Code, recorder.Body.String())
		}
	}
	if *calls != 2 {
		t.Errorf("calls = %d, want 2", *calls)
	}
}

// A form body that cannot be parsed might hide a method override, so an
// exempt POST carrying one is verified like any other request, and fails
// there: the token cannot be read either.
func TestExemptPostWithAnUnparsableFormIsRejected(t *testing.T) {
	t.Parallel()
	handler, calls := protected(t, csrf.Options{Exempt: []string{"POST /hooks/{provider}"}})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, formPost("/hooks/stripe", "event=%zz&_method=delete"))
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "csrf_invalid") || *calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, *calls, recorder.Body.String())
	}
	// Even with a valid cookie and token header the exemption does not apply:
	// the override check fails closed first, and the request is then verified.
	token := newToken(t)
	request := formPost("/hooks/stripe", "event=%zz")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	request.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || *calls != 0 {
		t.Fatalf("cross-site unparsable form: status=%d calls=%d", recorder.Code, *calls)
	}
}
