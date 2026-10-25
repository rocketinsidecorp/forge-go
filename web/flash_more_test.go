package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/web"
)

func TestSetFlashKeepsOtherCookiesAndRefusesWhatCannotFitInACookie(t *testing.T) {
	t.Parallel()
	var tooBig error
	handler := web.Flash(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "theme", Value: "dark"})
		if err := web.SetFlash(w, r, web.FlashNotice, "saved"); err != nil {
			t.Error(err)
		}
		// Three messages of the largest text do not fit a 2 KiB cookie.
		big := strings.Repeat("x", web.MaxFlashTextBytes)
		for _, kind := range []web.FlashKind{"one", "two", "three"} {
			if err := web.SetFlash(w, r, kind, big); err != nil {
				tooBig = err
				break
			}
		}
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/posts", nil))
	if tooBig == nil || !strings.Contains(tooBig.Error(), "encoded bytes") {
		t.Errorf("an oversized flash: %v", tooBig)
	}
	var theme, flash int
	for _, line := range recorder.Result().Header.Values("Set-Cookie") {
		switch {
		case strings.HasPrefix(line, "theme="):
			theme++
		case strings.HasPrefix(line, web.FlashCookieName+"="):
			flash++
		}
	}
	if theme != 1 || flash != 1 {
		t.Errorf("Set-Cookie lines: %d theme and %d flash, want one of each: %v", theme, flash, recorder.Result().Header.Values("Set-Cookie"))
	}
}
