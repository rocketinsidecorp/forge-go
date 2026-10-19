package web_test

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/web"
)

func flashCookieFrom(t *testing.T, response *http.Response) *http.Cookie {
	t.Helper()
	var found *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == web.FlashCookieName {
			if found != nil {
				t.Fatalf("flash cookie set twice: %v", response.Header.Values("Set-Cookie"))
			}
			found = cookie
		}
	}
	return found
}

func TestFlashRoundTripShowsMessagesOnce(t *testing.T) {
	t.Parallel()
	var shown []web.FlashMessage
	handler := web.Flash(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := web.SetFlash(w, r, web.FlashNotice, "first"); err != nil {
				t.Error(err)
			}
			if err := web.SetFlash(w, r, web.FlashAlert, "<b>careful</b>"); err != nil {
				t.Error(err)
			}
			if err := web.SetFlash(w, r, web.FlashNotice, "Post was created."); err != nil {
				t.Error(err)
			}
			_ = web.Redirect(w, "/posts/1")
			return
		}
		shown = web.Flashes(r.Context())
		if web.FlashText(r.Context(), web.FlashAlert) != "<b>careful</b>" && len(shown) > 0 {
			t.Errorf("FlashText alert = %q", web.FlashText(r.Context(), web.FlashAlert))
		}
	}))

	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/posts", nil))
	set := flashCookieFrom(t, post.Result())
	if set == nil || !set.Secure || !set.HttpOnly || set.SameSite != http.SameSiteLaxMode || set.Path != "/" || set.Domain != "" || set.MaxAge != 0 {
		t.Fatalf("flash cookie = %+v", set)
	}

	get := httptest.NewRequest(http.MethodGet, "/posts/1", nil)
	get.AddCookie(set)
	shownRecorder := httptest.NewRecorder()
	handler.ServeHTTP(shownRecorder, get)
	if len(shown) != 2 || shown[0] != (web.FlashMessage{Kind: web.FlashNotice, Text: "Post was created."}) || shown[1].Kind != web.FlashAlert {
		t.Fatalf("shown = %+v", shown)
	}
	cleared := flashCookieFrom(t, shownRecorder.Result())
	if cleared == nil || cleared.MaxAge >= 0 || cleared.Value != "" || !cleared.Secure || !cleared.HttpOnly {
		t.Fatalf("flash cookie was not cleared after it was read: %+v", cleared)
	}

	// Without the cookie (the browser applied the expiry) nothing is shown.
	shown = nil
	again := httptest.NewRecorder()
	handler.ServeHTTP(again, httptest.NewRequest(http.MethodGet, "/posts/1", nil))
	if len(shown) != 0 || flashCookieFrom(t, again.Result()) != nil {
		t.Fatalf("second request: shown=%+v cookies=%v", shown, again.Header().Values("Set-Cookie"))
	}
}

func TestFlashSetDuringDisplayReplacesExpiry(t *testing.T) {
	t.Parallel()
	handler := web.Flash(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := web.SetFlash(w, r, web.FlashNotice, "next"); err != nil {
			t.Error(err)
		}
	}))
	old := httptest.NewRecorder()
	web.Flash(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = web.SetFlash(w, r, web.FlashNotice, "previous")
	})).ServeHTTP(old, httptest.NewRequest(http.MethodPost, "/", nil))

	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.AddCookie(flashCookieFrom(t, old.Result()))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	cookie := flashCookieFrom(t, recorder.Result())
	if cookie == nil || cookie.MaxAge != 0 || cookie.Value == "" {
		t.Fatalf("the new flash must replace the expiry: %v", recorder.Header().Values("Set-Cookie"))
	}
}

func TestFlashDiscardsMalformedCookies(t *testing.T) {
	t.Parallel()
	encode := func(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
	for _, value := range []string{
		"not base64!",
		encode(`{"k":"notice"}`),
		encode(`[]`),
		encode(`[{"k":"Notice","t":"x"}]`),
		encode(`[{"k":"notice","t":"a"},{"k":"notice","t":"b"}]`),
		encode(`[{"k":"a","t":""},{"k":"b","t":""},{"k":"c","t":""},{"k":"d","t":""},{"k":"e","t":""}]`),
		encode(`[{"k":"notice","t":"` + strings.Repeat("a", web.MaxFlashTextBytes+1) + `"}]`),
		encode(`[{"k":"notice","t":"x"}]`) + "=",
		strings.Repeat("A", web.MaxFlashCookieBytes+1),
	} {
		var shown []web.FlashMessage
		handler := web.Flash(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { shown = web.Flashes(r.Context()) }))
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Cookie", web.FlashCookieName+"="+value)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if len(shown) != 0 {
			t.Fatalf("%q: shown %+v", value, shown)
		}
		if cleared := flashCookieFrom(t, recorder.Result()); cleared == nil || cleared.MaxAge >= 0 {
			t.Fatalf("%q: malformed cookie not cleared", value)
		}
	}
}

func TestSetFlashValidation(t *testing.T) {
	t.Parallel()
	if err := web.SetFlash(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), web.FlashNotice, "x"); !errors.Is(err, web.ErrFlashUnavailable) {
		t.Fatalf("without middleware: %v", err)
	}
	web.Flash(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, tt := range []struct {
			kind web.FlashKind
			text string
		}{
			{"", "x"}, {"Notice", "x"}, {"no-dash", "x"}, {web.FlashNotice, "\xff"}, {web.FlashNotice, strings.Repeat("a", web.MaxFlashTextBytes+1)},
		} {
			if err := web.SetFlash(w, r, tt.kind, tt.text); err == nil {
				t.Errorf("SetFlash(%q, %d bytes) succeeded", tt.kind, len(tt.text))
			}
		}
		for _, kind := range []web.FlashKind{"a", "b", "c", "d"} {
			if err := web.SetFlash(w, r, kind, strings.Repeat("é", 100)); err != nil {
				t.Errorf("SetFlash(%q): %v", kind, err)
			}
		}
		if err := web.SetFlash(w, r, "e", "x"); err == nil {
			t.Error("a fifth kind was accepted")
		}
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
}
