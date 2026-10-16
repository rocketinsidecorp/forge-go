package mail

import (
	"context"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/rocketinsidecorp/forge-go/config"
)

// Preview describes one email an application can render in the browser
// during development, with fixture data, without sending anything. It is the
// counterpart of Rails' ActionMailer::Preview.
type Preview struct {
	// Name is the URL segment: lower-case letters, digits, '-' and '_'.
	Name string
	// Description is shown in the index.
	Description string
	// Build returns the message to preview. It should use fixture data and
	// must not send mail or touch production services.
	Build func(ctx context.Context) (Message, error)
}

var previewName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// NewPreviewHandler serves the registered previews under prefix (for example
// "/_mail"): an index, and per preview the headers, the HTML part rendered in
// a sandboxed frame, the text part, the inline images and the raw MIME
// source. Messages go through the real encoder, so the page shows exactly the
// validation error production would hit.
//
// It is for development and takes two independent switches, both required:
// FORGE_ENV must be development or test, and FORGE_MAIL_PREVIEW=true must be
// set explicitly (cfg.Mail.Preview). The second one matters because an unset
// FORGE_ENV means development: a production container that lost its
// environment would otherwise serve the previews. Config validation rejects
// FORGE_MAIL_PREVIEW outside development and test. Register the handler on a
// subtree pattern:
//
//	if cfg.Mail.Preview {
//	    handler, err := mail.NewPreviewHandler(cfg, "/_mail", previews...)
//	    mux.Handle("GET /_mail/", handler)
//	}
//
// HTML previews are served with a restrictive Content-Security-Policy
// (no scripts, no forms, no navigation) and inside a sandboxed iframe, so
// even hostile template data cannot run script in the developer's browser.
func NewPreviewHandler(cfg config.Config, prefix string, previews ...Preview) (http.Handler, error) {
	if cfg.Environment != config.Development && cfg.Environment != config.Test {
		return nil, errors.New("mail: previews are available only in development and test")
	}
	if !cfg.Mail.Preview {
		return nil, errors.New("mail: previews need FORGE_MAIL_PREVIEW=true (explicit opt-in)")
	}
	if !strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") || strings.ContainsAny(prefix, "?#\\ ") {
		return nil, errors.New("mail: preview prefix must look like \"/_mail\"")
	}
	h := &previewHandler{prefix: prefix, byName: make(map[string]Preview, len(previews))}
	for _, preview := range previews {
		if !previewName.MatchString(preview.Name) {
			return nil, errors.New("mail: preview names must match [a-z0-9][a-z0-9_-]*")
		}
		if preview.Build == nil {
			return nil, errors.New("mail: preview Build is required")
		}
		if _, exists := h.byName[preview.Name]; exists {
			return nil, errors.New("mail: duplicate preview name")
		}
		h.byName[preview.Name] = preview
		h.order = append(h.order, preview.Name)
	}
	return h, nil
}

type previewHandler struct {
	prefix string
	byName map[string]Preview
	order  []string
}

func (h *previewHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, h.prefix)
	// The prefix must end at a path segment: "/_mailwelcome" is not under
	// "/_mail".
	if !ok || (rest != "" && rest[0] != '/') {
		http.NotFound(w, r)
		return
	}
	segments := strings.Split(strings.Trim(rest, "/"), "/")
	if rest == "" || rest == "/" {
		h.index(w)
		return
	}
	preview, found := h.byName[segments[0]]
	if !found {
		http.NotFound(w, r)
		return
	}
	msg, encoded, buildErr := h.build(r.Context(), preview)
	switch {
	case len(segments) == 1:
		h.page(w, preview, msg, encoded, buildErr)
	case buildErr != nil:
		http.Error(w, "this preview does not build; see the preview page", http.StatusUnprocessableEntity)
	case len(segments) == 2 && segments[1] == "html" && msg.HTML != "":
		h.html(w, preview, msg)
	case len(segments) == 2 && segments[1] == "text" && msg.Text != "":
		serve(w, "text/plain; charset=utf-8", []byte(msg.Text))
	case len(segments) == 2 && segments[1] == "raw":
		serve(w, "text/plain; charset=utf-8", encoded.Data)
	case len(segments) == 3 && segments[1] == "cid":
		h.inline(w, r, msg, segments[2])
	default:
		http.NotFound(w, r)
	}
}

func (h *previewHandler) build(ctx context.Context, preview Preview) (Message, Encoded, error) {
	msg, err := preview.Build(ctx)
	if err != nil {
		return Message{}, Encoded{}, fmt.Errorf("building the preview: %w", err)
	}
	encoded, err := Encode(msg)
	if err != nil {
		return msg, Encoded{}, err
	}
	return msg, encoded, nil
}

// security headers shared by every response. Previews never need script.
func secure(w http.ResponseWriter, csp string) {
	header := w.Header()
	header.Set("Content-Security-Policy", csp)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Frame-Options", "SAMEORIGIN")
}

const (
	pageCSP    = "default-src 'none'; style-src 'unsafe-inline'; frame-src 'self'; base-uri 'none'; form-action 'none'"
	contentCSP = "default-src 'none'; img-src 'self' data: https: http:; style-src 'unsafe-inline'; font-src https: http:; sandbox"
)

func serve(w http.ResponseWriter, contentType string, body []byte) {
	secure(w, contentCSP)
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(body)
}

// html serves the HTML part with cid: references pointing at the inline
// attachment route.
func (h *previewHandler) html(w http.ResponseWriter, preview Preview, msg Message) {
	body := msg.HTML
	for _, attachment := range msg.Attachments {
		if attachment.ContentID != "" {
			body = strings.ReplaceAll(body, "cid:"+attachment.ContentID, h.prefix+"/"+preview.Name+"/cid/"+url.PathEscape(attachment.ContentID))
		}
	}
	serve(w, "text/html; charset=utf-8", []byte(body))
}

func (h *previewHandler) inline(w http.ResponseWriter, r *http.Request, msg Message, id string) {
	for _, attachment := range msg.Attachments {
		if attachment.ContentID == id {
			contentType, err := attachmentContentType(attachment.ContentType)
			if err != nil {
				contentType = "application/octet-stream"
			}
			serve(w, contentType, attachment.Data)
			return
		}
	}
	http.NotFound(w, r)
}

var previewPage = htmltemplate.Must(htmltemplate.New("preview").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mail preview</title>
<style>
body{font:15px/1.5 system-ui,sans-serif;margin:0;color:#1b1f24;background:#f6f7f9}
main{max-width:60rem;margin:0 auto;padding:1.5rem 1rem}
a{color:#0b57d0}
h1{font-size:1.25rem;margin:.25rem 0 1rem}
table{border-collapse:collapse;background:#fff;width:100%;margin-bottom:1rem}
th,td{text-align:left;padding:.4rem .75rem;border-bottom:1px solid #e3e6ea;vertical-align:top}
th{width:9rem;color:#57606a;font-weight:600}
pre{background:#fff;border:1px solid #e3e6ea;padding:1rem;overflow:auto;white-space:pre-wrap}
iframe{width:100%;height:32rem;border:1px solid #e3e6ea;background:#fff}
.err{background:#fff1f0;border:1px solid #f4b4ae;padding:.75rem 1rem;border-radius:.375rem}
ul{padding-left:1.25rem}
</style></head><body><main>
{{if .Index}}
<h1>Mail previews</h1>
{{if .Items}}<ul>{{range .Items}}<li><a href="{{$.Prefix}}/{{.Name}}">{{.Name}}</a>{{if .Description}} &mdash; {{.Description}}{{end}}</li>{{end}}</ul>
{{else}}<p>No previews are registered.</p>{{end}}
{{else}}
<p><a href="{{.Prefix}}/">&larr; All previews</a></p>
<h1>{{.Name}}</h1>
{{if .Error}}<p class="err"><strong>This message would not be sent:</strong> {{.Error}}</p>{{end}}
{{if .Message}}
<table>
<tr><th>From</th><td>{{.From}}</td></tr>
{{with .ReplyTo}}<tr><th>Reply-To</th><td>{{.}}</td></tr>{{end}}
<tr><th>To</th><td>{{.To}}</td></tr>
{{with .Cc}}<tr><th>Cc</th><td>{{.}}</td></tr>{{end}}
{{with .Bcc}}<tr><th>Bcc (envelope only)</th><td>{{.}}</td></tr>{{end}}
<tr><th>Subject</th><td>{{.Message.Subject}}</td></tr>
{{if .MessageID}}<tr><th>Message-ID</th><td>{{.MessageID}}</td></tr>{{end}}
{{range .Attachments}}<tr><th>Attachment</th><td>{{.}}</td></tr>{{end}}
</table>
{{if .HasHTML}}<h2>HTML</h2><iframe sandbox src="{{.Prefix}}/{{.Name}}/html" title="HTML part"></iframe>{{end}}
{{if .HasText}}<h2>Text</h2><pre>{{.Message.Text}}</pre>{{end}}
{{if .MessageID}}<p><a href="{{.Prefix}}/{{.Name}}/raw">Raw MIME source</a></p>{{end}}
{{end}}
{{end}}
</main></body></html>`))

type previewView struct {
	Index       bool
	Prefix      string
	Items       []Preview
	Name        string
	Error       string
	Message     *Message
	MessageID   string
	From        string
	ReplyTo     string
	To, Cc, Bcc string
	Attachments []string
	HasHTML     bool
	HasText     bool
}

func (h *previewHandler) index(w http.ResponseWriter) {
	view := previewView{Index: true, Prefix: h.prefix}
	for _, name := range h.order {
		view.Items = append(view.Items, h.byName[name])
	}
	h.render(w, http.StatusOK, view)
}

func (h *previewHandler) page(w http.ResponseWriter, preview Preview, msg Message, encoded Encoded, buildErr error) {
	view := previewView{Prefix: h.prefix, Name: preview.Name}
	status := http.StatusOK
	if buildErr != nil {
		view.Error = buildErr.Error()
		status = http.StatusUnprocessableEntity
	}
	if buildErr == nil || msg.From.Email != "" || msg.Subject != "" || len(msg.To) > 0 {
		view.Message = &msg
		view.MessageID = encoded.MessageID
		view.From = msg.From.Name + " <" + msg.From.Email + ">"
		view.ReplyTo = join(msg.ReplyTo)
		view.To, view.Cc, view.Bcc = join(msg.To), join(msg.Cc), join(msg.Bcc)
		view.HasHTML = buildErr == nil && msg.HTML != ""
		view.HasText = msg.Text != ""
		for _, attachment := range msg.Attachments {
			label := fmt.Sprintf("%s (%d bytes)", attachment.Filename, len(attachment.Data))
			if attachment.ContentID != "" {
				label += ", inline as cid:" + attachment.ContentID
			}
			view.Attachments = append(view.Attachments, label)
		}
	}
	h.render(w, status, view)
}

func join(list []Address) string {
	parts := make([]string, len(list))
	for i, address := range list {
		if address.Name != "" {
			parts[i] = address.Name + " <" + address.Email + ">"
		} else {
			parts[i] = address.Email
		}
	}
	return strings.Join(parts, ", ")
}

func (h *previewHandler) render(w http.ResponseWriter, status int, view previewView) {
	secure(w, pageCSP)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var out strings.Builder
	if err := previewPage.Execute(&out, view); err != nil {
		http.Error(w, "preview page failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(out.String()))
}
