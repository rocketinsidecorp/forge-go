package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io"
	"io/fs"
	"strings"
	texttemplate "text/template"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// ErrTemplate is returned when a template fails to render. Rendering is
// buffered, so a failure never yields a partial message and Deliver sends
// nothing.
var ErrTemplate = fault.New("mail_template", "email template failed to render", fault.CategoryInternal, 0)

// File name suffixes of a template named "welcome":
//
//	welcome.subject.tmpl   text/template, optional, one line
//	welcome.text.tmpl      text/template, the text/plain part
//	welcome.html.gohtml    html/template, the text/html part
//
// At least one of the text and HTML files must exist. The HTML file uses the
// same *.gohtml convention and contextual escaping as package views.
const (
	subjectSuffix = ".subject.tmpl"
	textSuffix    = ".text.tmpl"
	htmlSuffix    = ".html.gohtml"
)

// Template renders the bodies of one kind of email from data of type T.
// Typing the data makes a template's inputs a compile-time contract between
// the caller and the template. A Template is immutable and safe for
// concurrent use.
//
// The HTML part is rendered with html/template, so every value is escaped
// for its context (element text, attribute, URL, script, CSS); the text and
// subject parts use text/template, where nothing is escaped, which is
// correct for plain text. Values placed in the subject are still validated
// as a header when the message is sent.
type Template[T any] struct {
	subject *texttemplate.Template
	text    *texttemplate.Template
	html    *htmltemplate.Template
	names   struct{ subject, text, html string }
}

// TemplateOption customises NewTemplate.
type TemplateOption func(*templateConfig)

type templateConfig struct {
	funcs        map[string]any
	textPartials []string
	htmlPartials []string
}

// WithFuncs adds template functions to every part. Functions must be
// deterministic and free of side effects.
func WithFuncs(funcs map[string]any) TemplateOption {
	return func(c *templateConfig) {
		if c.funcs == nil {
			c.funcs = make(map[string]any, len(funcs))
		}
		for name, fn := range funcs {
			c.funcs[name] = fn
		}
	}
}

// WithTextPartials parses the files matching the fs.Glob patterns into the
// subject and text templates, for shared {{define}} blocks. A pattern that
// matches nothing is an error.
func WithTextPartials(patterns ...string) TemplateOption {
	return func(c *templateConfig) { c.textPartials = append(c.textPartials, patterns...) }
}

// WithHTMLPartials parses the files matching the fs.Glob patterns into the
// HTML template, for a shared layout, header or footer.
func WithHTMLPartials(patterns ...string) TemplateOption {
	return func(c *templateConfig) { c.htmlPartials = append(c.htmlPartials, patterns...) }
}

// NewTemplate loads the template files for name from fsys (typically an
// embed.FS) and parses them, so syntax errors surface at start-up.
func NewTemplate[T any](fsys fs.FS, name string, options ...TemplateOption) (*Template[T], error) {
	if fsys == nil {
		return nil, errors.New("mail: template filesystem is required")
	}
	if name == "" || !fs.ValidPath(name) || name == "." {
		return nil, errors.New("mail: template name must be a relative slash-separated path")
	}
	var cfg templateConfig
	for _, option := range options {
		option(&cfg)
	}
	t := &Template[T]{}
	var err error
	if t.text, t.names.text, err = parseText(fsys, name+textSuffix, cfg.funcs, cfg.textPartials); err != nil {
		return nil, err
	}
	if t.subject, t.names.subject, err = parseText(fsys, name+subjectSuffix, cfg.funcs, cfg.textPartials); err != nil {
		return nil, err
	}
	if t.html, t.names.html, err = parseHTML(fsys, name+htmlSuffix, cfg.funcs, cfg.htmlPartials); err != nil {
		return nil, err
	}
	if t.html != nil {
		// html/template reports context errors (an unterminated attribute,
		// ambiguous branches) only when it first executes. Executing once
		// with the zero value surfaces them at start-up; execution errors
		// caused by the empty data are expected and ignored.
		var zero T
		var escapeErr *htmltemplate.Error
		if err := t.html.ExecuteTemplate(io.Discard, t.names.html, zero); errors.As(err, &escapeErr) {
			return nil, fmt.Errorf("mail: template %q: %w", name+htmlSuffix, err)
		}
	}
	if t.text == nil && t.html == nil {
		return nil, fmt.Errorf("mail: template %q has neither %s nor %s", name, name+textSuffix, name+htmlSuffix)
	}
	return t, nil
}

func exists(fsys fs.FS, file string) (bool, error) {
	if _, err := fs.Stat(fsys, file); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("mail: stat template %q: %w", file, err)
	}
	return true, nil
}

func parseText(fsys fs.FS, file string, funcs map[string]any, partials []string) (*texttemplate.Template, string, error) {
	ok, err := exists(fsys, file)
	if err != nil || !ok {
		return nil, "", err
	}
	base := file[strings.LastIndex(file, "/")+1:]
	tmpl := texttemplate.New(base).Option("missingkey=error")
	if funcs != nil {
		tmpl = tmpl.Funcs(funcs)
	}
	if len(partials) > 0 {
		if tmpl, err = tmpl.ParseFS(fsys, partials...); err != nil {
			return nil, "", fmt.Errorf("mail: parse text partials: %w", err)
		}
	}
	if tmpl, err = tmpl.ParseFS(fsys, file); err != nil {
		return nil, "", fmt.Errorf("mail: parse %q: %w", file, err)
	}
	return tmpl, base, nil
}

func parseHTML(fsys fs.FS, file string, funcs map[string]any, partials []string) (*htmltemplate.Template, string, error) {
	ok, err := exists(fsys, file)
	if err != nil || !ok {
		return nil, "", err
	}
	base := file[strings.LastIndex(file, "/")+1:]
	tmpl := htmltemplate.New(base).Option("missingkey=error")
	if funcs != nil {
		tmpl = tmpl.Funcs(funcs)
	}
	if len(partials) > 0 {
		if tmpl, err = tmpl.ParseFS(fsys, partials...); err != nil {
			return nil, "", fmt.Errorf("mail: parse html partials: %w", err)
		}
	}
	if tmpl, err = tmpl.ParseFS(fsys, file); err != nil {
		return nil, "", fmt.Errorf("mail: parse %q: %w", file, err)
	}
	return tmpl, base, nil
}

// Rendered is the output of Template.Render.
type Rendered struct {
	Subject string
	Text    string
	HTML    string
}

// Render executes every present part into memory. Any failure returns
// ErrTemplate and no output at all.
func (t *Template[T]) Render(data T) (Rendered, error) {
	var out Rendered
	if t.subject != nil {
		var buf bytes.Buffer
		if err := t.subject.ExecuteTemplate(&buf, t.names.subject, data); err != nil {
			return Rendered{}, ErrTemplate.WithCause(fmt.Errorf("subject: %w", err))
		}
		out.Subject = strings.TrimSpace(buf.String())
	}
	if t.text != nil {
		var buf bytes.Buffer
		if err := t.text.ExecuteTemplate(&buf, t.names.text, data); err != nil {
			return Rendered{}, ErrTemplate.WithCause(fmt.Errorf("text: %w", err))
		}
		out.Text = buf.String()
	}
	if t.html != nil {
		var buf bytes.Buffer
		if err := t.html.ExecuteTemplate(&buf, t.names.html, data); err != nil {
			return Rendered{}, ErrTemplate.WithCause(fmt.Errorf("html: %w", err))
		}
		out.HTML = buf.String()
	}
	return out, nil
}

// Message renders data into a copy of base: Text and HTML are set from the
// templates, and Subject too when a subject template exists and base.Subject
// is empty. Addresses, attachments and headers come from base.
func (t *Template[T]) Message(base Message, data T) (Message, error) {
	rendered, err := t.Render(data)
	if err != nil {
		return Message{}, err
	}
	msg := base.Clone()
	if rendered.Subject != "" && msg.Subject == "" {
		msg.Subject = rendered.Subject
	}
	if t.text != nil {
		msg.Text = rendered.Text
	}
	if t.html != nil {
		msg.HTML = rendered.HTML
	}
	return msg, nil
}

// Deliver renders and sends. Rendering happens first and completely, so a
// template error means nothing was sent.
func (t *Template[T]) Deliver(ctx context.Context, sender Sender, base Message, data T) error {
	if sender == nil {
		return errors.New("mail: sender is required")
	}
	msg, err := t.Message(base, data)
	if err != nil {
		return err
	}
	return sender.Send(ctx, msg)
}
