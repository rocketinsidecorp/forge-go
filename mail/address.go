package mail

import (
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	netmail "net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/fault"
)

// ErrAddressUnsupported matches (errors.Is) an address that is plausible in
// the wild but outside what Forge implements: one with non-ASCII characters
// in the local part or domain (SMTPUTF8 and internationalised domain names
// are not supported). It is reported distinctly from ErrInvalid so
// applications and logs can tell "cannot deliver to this person" from
// "malformed input". The non-ASCII form is never converted to punycode
// automatically: a wrong conversion would deliver to someone else.
var ErrAddressUnsupported = fault.New("mail_address_unsupported", "email address is not supported", fault.CategoryInvalid, 0)

// Address is one mailbox: an optional display name and an ASCII addr-spec.
//
// Forge does not implement SMTPUTF8 (RFC 6531) or IDNA: the address itself
// must be ASCII, while the display name may be any text and is encoded per
// RFC 2047. Only the dot-atom form of the local part is accepted; quoted
// local parts, comments, route addresses and domain literals are rejected,
// which removes the ambiguity attackers use to smuggle extra recipients or
// commands. The rules are defined once, in config.ValidateMailbox, and shared
// with configuration validation.
type Address struct {
	Name  string
	Email string
}

// NewAddress validates and returns an address.
func NewAddress(name, email string) (Address, error) {
	address := Address{Name: name, Email: email}
	if err := address.Validate(); err != nil {
		return Address{}, err
	}
	return address, nil
}

// ParseAddress parses "user@example.com" or "Display Name <user@example.com>"
// with net/mail and applies Forge's stricter validation to the result.
func ParseAddress(value string) (Address, error) {
	name, email, err := config.ParseMailbox(value)
	if err != nil {
		return Address{}, mailboxFault("address", err)
	}
	return Address{Name: name, Email: email}, nil
}

// Validate reports why the address cannot be used in a message.
func (a Address) Validate() error { return a.validate("address") }

// String returns the header form: name RFC 2047 encoded when needed, followed
// by <email>. An address without a name renders as the bare addr-spec. It is
// only meaningful for a valid address.
func (a Address) String() string {
	switch {
	case a.Name == "":
		return a.Email
	case strings.Contains(a.Name, "=?"):
		// Plain text that looks like an encoded-word could be decoded by a
		// client into something other than what was written.
		return encodeText(a.Name) + " <" + a.Email + ">"
	default:
		return (&netmail.Address{Name: a.Name, Address: a.Email}).String()
	}
}

// Domain returns the part after the "@".
func (a Address) Domain() string {
	_, domain, _ := strings.Cut(a.Email, "@")
	return domain
}

func (a Address) validate(field string) error {
	if err := config.ValidateMailbox(a.Name, a.Email); err != nil {
		return mailboxFault(field, err)
	}
	return nil
}

func mailboxFault(field string, err error) error {
	var mailbox *config.MailboxError
	if !errors.As(err, &mailbox) {
		return invalid(field, "is not a valid mailbox")
	}
	if mailbox.Unsupported {
		out := ErrAddressUnsupported.WithCause(nil)
		out.Message = field + " " + mailbox.Reason
		out.Metadata = map[string]string{"field": field}
		return out
	}
	return invalid(field, mailbox.Reason)
}

// rejectControl refuses every character that could end or split a header
// line (CR, LF, NUL and every other control character, plus the Unicode line
// and paragraph separators some clients treat as line breaks) and the
// bidirectional overrides and isolates, which reorder displayed text.
func rejectControl(field, value string) error {
	for _, r := range value {
		if unicode.IsControl(r) || isBidiControl(r) || r == rune(0x2028) || r == rune(0x2029) || r == utf8.RuneError {
			return invalid(field, "contains a control, line-break or direction-override character")
		}
	}
	return nil
}

func addressList(field string, list []Address) error {
	for i, address := range list {
		if err := address.validate(fmt.Sprintf("%s[%d]", field, i)); err != nil {
			return err
		}
	}
	return nil
}

func formatAddresses(list []Address) []string {
	out := make([]string, len(list))
	for i, address := range list {
		out[i] = address.String()
	}
	return out
}

// encodeText renders unstructured header text (a subject, a display name).
// Text that is not plain printable ASCII is Q-encoded per RFC 2047. Text
// that is plain ASCII but contains "=?" is encoded too, entirely, because a
// client would otherwise decode a hand-written "=?utf-8?b?...?=" into
// attacker-chosen text, bypassing every check applied to the literal value.
func encodeText(s string) string {
	if !strings.Contains(s, "=?") {
		return mime.QEncoding.Encode("utf-8", s)
	}
	const maxRaw = 45 // 45 bytes -> 60 base64 characters, 72 with the delimiters
	var words []string
	for len(s) > 0 {
		n := min(maxRaw, len(s))
		for n < len(s) && !utf8.RuneStart(s[n]) {
			n--
		}
		words = append(words, "=?utf-8?b?"+base64.StdEncoding.EncodeToString([]byte(s[:n]))+"?=")
		s = s[n:]
	}
	return strings.Join(words, " ")
}
