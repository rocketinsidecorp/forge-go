package views

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

type failingResponse struct {
	header http.Header
	status int
}

func (w *failingResponse) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *failingResponse) WriteHeader(status int)    { w.status = status }
func (w *failingResponse) Write([]byte) (int, error) { return 0, errors.New("client went away") }

func TestRendererRefusesMisuseAndReportsAClientThatLeft(t *testing.T) {
	t.Parallel()
	renderer, err := New(fstest.MapFS{"hello.gohtml": {Data: []byte("<p>{{.}}</p>")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(fstest.MapFS{"readme.txt": {Data: []byte("not a template")}}); err == nil || !strings.Contains(err.Error(), "parse templates") {
		t.Errorf("a filesystem without templates: %v", err)
	}
	var nilRenderer *Renderer
	if err := nilRenderer.Render(httptest.NewRecorder(), "hello.gohtml", 200, "x"); err == nil || !strings.Contains(err.Error(), "renderer is required") {
		t.Errorf("a nil renderer: %v", err)
	}
	if err := (&Renderer{}).Render(httptest.NewRecorder(), "hello.gohtml", 200, "x"); err == nil || !strings.Contains(err.Error(), "renderer is required") {
		t.Errorf("an empty renderer: %v", err)
	}
	if err := renderer.Render(httptest.NewRecorder(), "", 200, "x"); err == nil || !strings.Contains(err.Error(), "template name is required") {
		t.Errorf("an empty template name: %v", err)
	}
	if err := renderer.Render(httptest.NewRecorder(), "hello.gohtml", 99, "x"); err == nil {
		t.Error("an invalid status was accepted")
	}
	recorder := httptest.NewRecorder()
	if err := renderer.Render(recorder, "hello.gohtml", 0, "x"); err != nil || recorder.Code != http.StatusOK {
		t.Errorf("a zero status: %v, code %d", err, recorder.Code)
	}
	failing := &failingResponse{}
	if err := renderer.Render(failing, "hello.gohtml", http.StatusCreated, "x"); err == nil || !strings.Contains(err.Error(), "write response") {
		t.Errorf("a client that left: %v", err)
	}
	if failing.status != http.StatusCreated {
		t.Errorf("status = %d", failing.status)
	}
}
