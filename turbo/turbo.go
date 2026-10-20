// Package turbo builds Turbo Streams: <turbo-stream> elements that tell the
// browser to append, prepend, replace, update, remove or insert beside an
// element identified by its DOM id.
//
// The same elements are returned as the response to a form submission
// (Render, when Requested) and pushed to every subscriber of a channel over
// Server-Sent Events (package broadcast). Their content is template.HTML
// produced by html/template, normally with views.Renderer.Fragment, so values
// are escaped the way they are on a page. Targets are validated DOM ids and
// cannot inject attributes.
//
//	row, _ := renderer.Fragment(r, "posts/_post", post)
//	stream, _ := turbo.Append("posts", row)
//	if turbo.Requested(r) {
//		return turbo.Render(w, http.StatusOK, stream)
//	}
//	return web.Redirect(w, "/posts")
package turbo

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"

	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

// ContentType is the media type Turbo requires for stream responses.
const ContentType = "text/vnd.turbo-stream.html"

// MaxTargetBytes is the longest DOM id a stream may target.
const MaxTargetBytes = 128

// ErrInvalidTarget is returned for a target that is not a valid DOM id.
var ErrInvalidTarget = errors.New("turbo: target must match [A-Za-z][A-Za-z0-9_:-]* and be at most 128 bytes")

// Action is what a stream does to its target.
type Action string

// The stream actions.
const (
	// ActionAppend adds the content at the end of the target's children.
	ActionAppend Action = "append"
	// ActionPrepend adds the content at the start of the target's children.
	ActionPrepend Action = "prepend"
	// ActionReplace replaces the target element itself.
	ActionReplace Action = "replace"
	// ActionUpdate replaces the target element's children.
	ActionUpdate Action = "update"
	// ActionRemove removes the target element; it takes no content.
	ActionRemove Action = "remove"
	// ActionBefore inserts the content before the target element.
	ActionBefore Action = "before"
	// ActionAfter inserts the content after the target element.
	ActionAfter Action = "after"
)

var targetPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_:-]{0,127}$`)

// ValidTarget reports whether target is a DOM id a stream may name. The
// character set contains nothing that can close an attribute, open a tag or
// start an entity, so a valid target needs no further escaping.
func ValidTarget(target string) error {
	if !targetPattern.MatchString(target) {
		return ErrInvalidTarget
	}
	return nil
}

var (
	domPrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	domIDPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// DomID returns the DOM id of a record, like Rails' dom_id: prefix_ID, or
// new_prefix for a zero UUID (a record that does not exist yet). prefix is a
// lowercase name ("post"); id is a uuid.UUID, a string of letters, digits, '_'
// and '-', or an integer. The result is always a valid stream target.
func DomID(prefix string, id any) (string, error) {
	if !domPrefixPattern.MatchString(prefix) {
		return "", fmt.Errorf("domID: invalid prefix %q", prefix)
	}
	var value string
	switch typed := id.(type) {
	case uuid.UUID:
		if typed == (uuid.UUID{}) {
			return "new_" + prefix, nil
		}
		value = typed.String()
	case string:
		value = typed
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		value = fmt.Sprint(typed)
	default:
		return "", fmt.Errorf("domID: unsupported id type %T", id)
	}
	if !domIDPattern.MatchString(value) {
		return "", errors.New("domID: id must be letters, digits, '_' or '-'")
	}
	return prefix + "_" + value, nil
}

// Stream is one <turbo-stream> element.
type Stream struct {
	action  Action
	target  string
	content template.HTML
}

// New builds a stream. The content is trusted markup (html/template output);
// ActionRemove must have none.
func New(action Action, target string, content template.HTML) (Stream, error) {
	switch action {
	case ActionAppend, ActionPrepend, ActionReplace, ActionUpdate, ActionBefore, ActionAfter:
	case ActionRemove:
		if content != "" {
			return Stream{}, errors.New("turbo: the remove action does not take content")
		}
	default:
		return Stream{}, fmt.Errorf("turbo: unknown action %q", string(action))
	}
	if err := ValidTarget(target); err != nil {
		return Stream{}, err
	}
	return Stream{action: action, target: target, content: content}, nil
}

// Append adds content at the end of the target's children.
func Append(target string, content template.HTML) (Stream, error) {
	return New(ActionAppend, target, content)
}

// Prepend adds content at the start of the target's children.
func Prepend(target string, content template.HTML) (Stream, error) {
	return New(ActionPrepend, target, content)
}

// Replace replaces the target element with content.
func Replace(target string, content template.HTML) (Stream, error) {
	return New(ActionReplace, target, content)
}

// Update replaces the target's children with content. Empty content clears
// the target.
func Update(target string, content template.HTML) (Stream, error) {
	return New(ActionUpdate, target, content)
}

// Remove removes the target element.
func Remove(target string) (Stream, error) { return New(ActionRemove, target, "") }

// Before inserts content before the target element.
func Before(target string, content template.HTML) (Stream, error) {
	return New(ActionBefore, target, content)
}

// After inserts content after the target element.
func After(target string, content template.HTML) (Stream, error) {
	return New(ActionAfter, target, content)
}

// Action returns the stream's action.
func (s Stream) Action() Action { return s.action }

// Target returns the DOM id the stream acts on.
func (s Stream) Target() string { return s.target }

// HTML returns the element:
//
//	<turbo-stream action="append" target="posts"><template>...</template></turbo-stream>
func (s Stream) HTML() template.HTML {
	var out strings.Builder
	s.writeTo(&out)
	// #nosec G203 -- the action is a fixed constant, the target matches targetPattern, and the content is already html/template output.
	return template.HTML(out.String())
}

func (s Stream) writeTo(out *strings.Builder) {
	out.WriteString(`<turbo-stream action="`)
	out.WriteString(string(s.action))
	out.WriteString(`" target="`)
	out.WriteString(s.target)
	out.WriteString(`">`)
	if s.action != ActionRemove {
		out.WriteString("<template>")
		out.WriteString(string(s.content))
		out.WriteString("</template>")
	}
	out.WriteString("</turbo-stream>\n")
}

// Join renders streams in order as one body.
func Join(streams ...Stream) template.HTML {
	var out strings.Builder
	for _, stream := range streams {
		stream.writeTo(&out)
	}
	// #nosec G203 -- every element was built by writeTo from validated parts.
	return template.HTML(out.String())
}

// Requested reports whether the client asked for a Turbo Stream response:
// the Accept header names text/vnd.turbo-stream.html (Turbo adds it to every
// form submission). A browser navigation, "*/*" and API clients do not.
func Requested(r *http.Request) bool {
	return web.AcceptsExplicitly(r, ContentType) && web.Negotiate(r, ContentType, "text/html") != ""
}

// Render writes streams as a Turbo Stream response with the given status.
// Turbo processes stream responses to form submissions; use 200 for a
// successful submission (or a redirect when the client did not ask for a
// stream) and 422 for a form that failed validation. Nothing is written when
// there are no streams.
func Render(w http.ResponseWriter, status int, streams ...Stream) error {
	if len(streams) == 0 {
		return errors.New("turbo: at least one stream is required")
	}
	if status < 100 || status > 999 {
		return fmt.Errorf("turbo: invalid HTTP status %d", status)
	}
	header := w.Header()
	header.Set("Content-Type", ContentType+"; charset=utf-8")
	header.Add("Vary", "Accept")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(Join(streams...))); err != nil {
		return fmt.Errorf("turbo: write response: %w", err)
	}
	return nil
}
