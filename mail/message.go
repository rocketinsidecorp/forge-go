package mail

import (
	"fmt"
	"mime"
	"net/textproto"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// Message is one outgoing email. It is plain data: every field is validated
// when the message is encoded (Encode, and therefore every Sender), so a
// Message built from untrusted input can never inject headers, recipients or
// MIME parts.
type Message struct {
	From    Address
	ReplyTo []Address
	To      []Address
	Cc      []Address
	// Bcc recipients receive the message but appear in no header.
	Bcc     []Address
	Subject string
	// Text and HTML are the bodies. At least one is required; with both, the
	// message is multipart/alternative and the HTML part is preferred by
	// clients.
	Text string
	HTML string
	// Attachments are sent as files, or as inline images referenced from the
	// HTML body by cid: when ContentID is set.
	Attachments []Attachment
	// Headers are extra headers from the AllowedHeaders list. Anything else
	// (including Bcc, To, Subject, Content-*) is rejected.
	Headers map[string]string
}

// Attachment is a file carried by a message.
type Attachment struct {
	// Filename is sanitised to a base name (no directories) and may not
	// contain control or bidirectional-override characters.
	Filename string
	// ContentType defaults to application/octet-stream. Multipart and message
	// types, parameters and types longer than 127 bytes are rejected.
	ContentType string
	Data        []byte
	// ContentID, when set, makes the attachment inline and addressable as
	// <img src="cid:ContentID"> from the HTML body. Allowed characters are
	// letters, digits and ._-@ .
	ContentID string
}

// Limits bound the size of a message. A zero field takes its default.
type Limits struct {
	MaxSubjectRunes    int
	MaxRecipients      int
	MaxBodyBytes       int
	MaxAttachments     int
	MaxAttachmentBytes int
	// MaxMessageBytes bounds the sum of bodies and attachments before
	// encoding; base64 grows attachments by about a third.
	MaxMessageBytes int
}

// DefaultLimits returns the limits used when none are configured: 255-rune
// subjects, 100 recipients, 2 MiB per body, 10 attachments of at most 7 MiB
// each and 10 MiB of content in total (about 13.7 MB once base64 encoded,
// below the 25 MB most providers accept). The encoder holds a message in
// memory, so large values multiply across concurrent sends; raise them
// deliberately with WithLimits and send big files as links.
func DefaultLimits() Limits {
	return Limits{
		MaxSubjectRunes:    255,
		MaxRecipients:      100,
		MaxBodyBytes:       2 << 20,
		MaxAttachments:     10,
		MaxAttachmentBytes: 7 << 20,
		MaxMessageBytes:    10 << 20,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxSubjectRunes > 0 {
		d.MaxSubjectRunes = l.MaxSubjectRunes
	}
	if l.MaxRecipients > 0 {
		d.MaxRecipients = l.MaxRecipients
	}
	if l.MaxBodyBytes > 0 {
		d.MaxBodyBytes = l.MaxBodyBytes
	}
	if l.MaxAttachments > 0 {
		d.MaxAttachments = l.MaxAttachments
	}
	if l.MaxAttachmentBytes > 0 {
		d.MaxAttachmentBytes = l.MaxAttachmentBytes
	}
	if l.MaxMessageBytes > 0 {
		d.MaxMessageBytes = l.MaxMessageBytes
	}
	return d
}

// allowedHeaders are the only extra headers a Message may set. They are
// ASCII, structured and do not alter routing or MIME structure.
var allowedHeaders = map[string]bool{
	"List-Unsubscribe":      true,
	"List-Unsubscribe-Post": true,
	"List-Id":               true,
	"Auto-Submitted":        true,
	"Precedence":            true,
	"In-Reply-To":           true,
	"References":            true,
}

// AllowedHeaders returns the extra headers Message.Headers accepts, sorted.
func AllowedHeaders() []string {
	names := make([]string, 0, len(allowedHeaders))
	for name := range allowedHeaders {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

const maxHeaderValueBytes = 900

// Errors returned for invalid input. Details name the field and the rule,
// never the offending value, so logs cannot be forged through them.
var (
	// ErrInvalid matches (errors.Is) every message validation failure.
	ErrInvalid = fault.New("mail_invalid", "invalid email message", fault.CategoryInvalid, 0)
)

func invalid(field, reason string) error {
	err := fault.New("mail_invalid", field+" "+reason, fault.CategoryInvalid, 0)
	err.Metadata = map[string]string{"field": field}
	return err
}

// Clone returns a deep copy, so a recorded or queued message cannot be
// mutated through the caller's slices and maps.
func (m Message) Clone() Message {
	out := m
	out.ReplyTo = slices.Clone(m.ReplyTo)
	out.To = slices.Clone(m.To)
	out.Cc = slices.Clone(m.Cc)
	out.Bcc = slices.Clone(m.Bcc)
	if m.Headers != nil {
		out.Headers = make(map[string]string, len(m.Headers))
		for k, v := range m.Headers {
			out.Headers[k] = v
		}
	}
	if m.Attachments != nil {
		out.Attachments = make([]Attachment, len(m.Attachments))
		for i, a := range m.Attachments {
			a.Data = slices.Clone(a.Data)
			out.Attachments[i] = a
		}
	}
	return out
}

// Recipients returns the SMTP envelope recipients (To, Cc, Bcc), lower-cased
// and de-duplicated, in order of first appearance.
func (m Message) Recipients() []string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range [][]Address{m.To, m.Cc, m.Bcc} {
		for _, address := range list {
			key := strings.ToLower(address.Email)
			if !seen[key] {
				seen[key] = true
				out = append(out, address.Email)
			}
		}
	}
	return out
}

// Validate checks the message against the default limits.
func (m Message) Validate() error { return m.validate(DefaultLimits()) }

func (m Message) validate(limits Limits) error {
	if err := m.From.validate("From"); err != nil {
		return err
	}
	for _, list := range []struct {
		name string
		list []Address
	}{{"ReplyTo", m.ReplyTo}, {"To", m.To}, {"Cc", m.Cc}, {"Bcc", m.Bcc}} {
		if err := addressList(list.name, list.list); err != nil {
			return err
		}
	}
	if len(m.ReplyTo) > 10 {
		return invalid("ReplyTo", "has too many addresses")
	}
	if len(m.Recipients()) == 0 {
		return invalid("To", "at least one recipient is required")
	}
	if len(m.To)+len(m.Cc)+len(m.Bcc) > limits.MaxRecipients {
		return invalid("recipients", "exceed the configured limit")
	}
	if err := validateSubject(m.Subject, limits.MaxSubjectRunes); err != nil {
		return err
	}
	if m.Text == "" && m.HTML == "" {
		return invalid("Text", "or HTML body is required")
	}
	if len(m.Text) > limits.MaxBodyBytes || len(m.HTML) > limits.MaxBodyBytes {
		return invalid("body", "exceeds the configured limit")
	}
	if !utf8.ValidString(m.Text) || !utf8.ValidString(m.HTML) {
		return invalid("body", "is not valid UTF-8")
	}
	if err := validateHeaders(m.Headers); err != nil {
		return err
	}
	return m.validateAttachments(limits)
}

func validateSubject(subject string, maxRunes int) error {
	if err := rejectControl("Subject", subject); err != nil {
		return err
	}
	if utf8.RuneCountInString(subject) > maxRunes {
		return invalid("Subject", "is too long")
	}
	return nil
}

func validateHeaders(headers map[string]string) error {
	seen := make(map[string]bool, len(headers))
	for name, value := range headers {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if !allowedHeaders[canonical] {
			return invalid("Headers", "contains a header that is not allowed")
		}
		if seen[canonical] {
			return invalid("Headers", "repeats a header")
		}
		seen[canonical] = true
		if value == "" || len(value) > maxHeaderValueBytes {
			return invalid("Headers."+canonical, "value is empty or too long")
		}
		for i := 0; i < len(value); i++ {
			if value[i] < 0x20 || value[i] > 0x7e {
				return invalid("Headers."+canonical, "value must be printable ASCII")
			}
		}
	}
	return nil
}

func (m Message) validateAttachments(limits Limits) error {
	if len(m.Attachments) > limits.MaxAttachments {
		return invalid("Attachments", "exceed the configured count")
	}
	total := len(m.Text) + len(m.HTML)
	ids := make(map[string]bool)
	for i, attachment := range m.Attachments {
		field := fmt.Sprintf("Attachments[%d]", i)
		if _, err := SanitizeFilename(attachment.Filename); err != nil {
			return invalid(field+".Filename", "is not acceptable")
		}
		if _, err := attachmentContentType(attachment.ContentType); err != nil {
			return invalid(field+".ContentType", "is not acceptable")
		}
		if len(attachment.Data) == 0 {
			return invalid(field+".Data", "is empty")
		}
		if len(attachment.Data) > limits.MaxAttachmentBytes {
			return invalid(field+".Data", "exceeds the configured limit")
		}
		total += len(attachment.Data)
		if attachment.ContentID != "" {
			if !validContentID(attachment.ContentID) || ids[attachment.ContentID] {
				return invalid(field+".ContentID", "is invalid or repeated")
			}
			ids[attachment.ContentID] = true
			if m.HTML == "" {
				return invalid(field+".ContentID", "requires an HTML body")
			}
		}
	}
	if total > limits.MaxMessageBytes {
		return invalid("message", "exceeds the configured size")
	}
	return nil
}

func validContentID(id string) bool {
	if len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("._-@", rune(c)) {
			return false
		}
	}
	return id != ""
}

// SanitizeFilename reduces name to a safe base name. Directory components
// ("../../etc/passwd", "C:\x\y.txt") are dropped and leading dots and
// surrounding spaces trimmed. Names containing control characters, line
// breaks or bidirectional overrides (which can disguise "fdp.exe" as
// ".pdf") are rejected rather than silently repaired, as is a name that is
// empty after sanitising or longer than 255 bytes.
func SanitizeFilename(name string) (string, error) {
	if name == "" || !utf8.ValidString(name) {
		return "", invalid("filename", "is empty or not valid UTF-8")
	}
	for _, r := range name {
		if unicode.IsControl(r) || isBidiControl(r) || r == '\u2028' || r == '\u2029' || r == utf8.RuneError {
			return "", invalid("filename", "contains a control or direction-override character")
		}
	}
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.Trim(name, " .")
	if name == "" || name == "/" || len(name) > 255 {
		return "", invalid("filename", "is empty or too long after sanitising")
	}
	return name, nil
}

func isBidiControl(r rune) bool {
	switch {
	case r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
		return true
	}
	return r == '\u200e' || r == '\u200f' || r == '\u061c'
}

// maxMediaTypeBytes caps an attachment's media type (RFC 6838 limits each of
// type and subtype to 127 bytes): with a file name of at most 255 bytes, a
// part header then stays far below the 998-byte line limit.
const maxMediaTypeBytes = 127

// attachmentContentType returns a canonical media type for the attachment.
func attachmentContentType(value string) (string, error) {
	if value == "" {
		return "application/octet-stream", nil
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || len(params) > 0 {
		return "", invalid("content type", "is malformed")
	}
	if len(mediaType) > maxMediaTypeBytes {
		return "", invalid("content type", "is too long")
	}
	if strings.HasPrefix(mediaType, "multipart/") || strings.HasPrefix(mediaType, "message/") {
		return "", invalid("content type", "multipart and message types are not allowed")
	}
	return mediaType, nil
}
