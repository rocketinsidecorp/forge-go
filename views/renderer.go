// Package views renders server-side HTML templates with contextual escaping.
package views

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
)

// Renderer is an immutable, concurrency-safe collection of HTML templates.
// New builds a flat collection; Load builds pages with layouts, partials and
// request-aware helpers.
type Renderer struct {
	templates *template.Template
	pages     map[string]*page
	fragments map[string]*page
	csp       string
}

// New parses every *.gohtml file in templates. Template functions and
// template names remain explicit in application code; no global registry is
// modified.
func New(templates fs.FS) (*Renderer, error) {
	if templates == nil {
		return nil, errors.New("views: template filesystem is required")
	}
	parsed, err := template.ParseFS(templates, "*.gohtml")
	if err != nil {
		return nil, fmt.Errorf("views: parse templates: %w", err)
	}
	if len(parsed.Templates()) == 0 {
		return nil, errors.New("views: no templates found; expected *.gohtml")
	}
	return &Renderer{templates: parsed}, nil
}

// Render executes name before writing any response bytes, so template errors
// cannot leave a partial HTML response. html/template applies contextual
// escaping to all data values. A non-positive status defaults to 200.
//
// For a Renderer built by Load, name is a page name such as "posts/index";
// pages that use the csrf or flash helpers need RenderRequest.
func (r *Renderer) Render(w http.ResponseWriter, name string, status int, data any) error {
	return r.RenderRequest(w, nil, name, status, data)
}

// RenderRequest is Render with the current request, which the csrf and flash
// helpers of a loaded Renderer read. A Renderer built by New ignores the
// request. Pages of a loaded Renderer are sent with its Content-Security-
// Policy. Render a form again with http.StatusUnprocessableEntity when its
// input is invalid.
func (r *Renderer) RenderRequest(w http.ResponseWriter, request *http.Request, name string, status int, data any) error {
	if r == nil || (r.templates == nil && r.pages == nil) {
		return errors.New("views: renderer is required")
	}
	if name == "" {
		return errors.New("views: template name is required")
	}
	if r.pages != nil {
		return r.renderPage(w, request, name, status, data)
	}
	if err := validStatus(status); err != nil {
		return err
	}
	if status <= 0 {
		status = http.StatusOK
	}
	var body bytes.Buffer
	if err := r.templates.ExecuteTemplate(&body, name, data); err != nil {
		return fmt.Errorf("views: execute %q: %w", name, err)
	}
	return writeHTML(w, status, body.Bytes())
}

func validStatus(status int) error {
	if status > 0 && (status < 100 || status > 999) {
		return fmt.Errorf("views: invalid HTTP status %d", status)
	}
	return nil
}

func writeHTML(w http.ResponseWriter, status int, body []byte) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("views: write response: %w", err)
	}
	return nil
}
