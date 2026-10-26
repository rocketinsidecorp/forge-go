package csrf

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An error that is not a public fault (nothing the package returns today)
// is answered as the generic invalid-token rejection, never with its text.
func TestRejectHidesErrorsThatAreNotFaults(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "https://app.example.com/posts", nil)
	recorder := httptest.NewRecorder()
	reject(recorder, request, errors.New("secret internal detail"))
	body := recorder.Body.String()
	if recorder.Code != http.StatusForbidden || !strings.Contains(body, ErrInvalid.Code) || strings.Contains(body, "secret") {
		t.Fatalf("status=%d body=%s", recorder.Code, body)
	}
}
