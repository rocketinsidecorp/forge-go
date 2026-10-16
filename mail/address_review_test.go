package mail_test

import (
	"errors"
	"mime"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

// Non-ASCII (SMTPUTF8/IDN) addresses cannot be delivered. That must be a
// distinct, explicit failure rather than a generic "invalid".
func TestNonASCIIAddressesAreReportedAsUnsupported(t *testing.T) {
	t.Parallel()
	for _, email := range []string{"ação@example.com", "joao@exämple.com", "用户@例子.广告", "a@xn--bcher-kva.example.comé"} {
		_, err := forgemail.NewAddress("", email)
		if !errors.Is(err, forgemail.ErrAddressUnsupported) {
			t.Errorf("NewAddress(%q) = %v, want ErrAddressUnsupported", email, err)
		}
		if errors.Is(err, forgemail.ErrInvalid) {
			t.Errorf("%q must not also match ErrInvalid", email)
		}
		if _, err := forgemail.ParseAddress("Name <" + email + ">"); !errors.Is(err, forgemail.ErrAddressUnsupported) {
			t.Errorf("ParseAddress(%q) = %v", email, err)
		}
		msg := baseMessage()
		msg.To[0].Email = email
		if _, err := forgemail.Encode(msg); !errors.Is(err, forgemail.ErrAddressUnsupported) {
			t.Errorf("Encode(%q) = %v", email, err)
		}
		if strings.Contains(err.Error(), email) {
			t.Errorf("error echoes the address")
		}
	}
	// Other violations stay plain validation errors.
	for _, email := range []string{"a b@example.com", "a@@example.com", "a@exa_mple.com"} {
		if _, err := forgemail.NewAddress("", email); !errors.Is(err, forgemail.ErrInvalid) {
			t.Errorf("NewAddress(%q) = %v, want ErrInvalid", email, err)
		}
	}
	// Non-ASCII display names remain supported.
	if _, err := forgemail.NewAddress("José Ação", "jose@example.com"); err != nil {
		t.Fatal(err)
	}
}

// The configuration check and the sender must agree: anything FORGE_MAIL_FROM
// accepts must be usable as a From address, and vice versa.
func TestConfigAndMailAgreeOnMailboxes(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"app@example.com", "App <app@example.com>", `"App, Inc" <app@example.com>`, "José <jose@example.com>",
		`"quoted local"@example.com`, "ação@example.com", "app@exämple.com", "Name\u202e <a@example.com>",
		"a@example.com\r\nBcc: x@y.com", "a@[127.0.0.1]", "<a@example.com>", "a@b@c", "not an address", "",
	} {
		_, _, configErr := config.ParseMailbox(value)
		_, mailErr := forgemail.ParseAddress(value)
		if (configErr == nil) != (mailErr == nil) {
			t.Errorf("%q: config=%v mail=%v", value, configErr, mailErr)
		}
	}
}

// ASCII text that looks like an RFC 2047 encoded-word would be decoded by a
// client into text chosen by the attacker, bypassing validation of the
// literal value, so it is encoded as a whole.
func TestEncodedWordLookalikesAreNeutralised(t *testing.T) {
	t.Parallel()
	attack := "=?utf-8?b?QmNjOiBldmlsQGV4YW1wbGUubmV0?=" // decodes to "Bcc: evil@example.net"
	for _, value := range []string{attack, "Re: " + attack, "x =?utf-8?q?y?= z", "=?", strings.TrimSpace(strings.Repeat("=?a ", 50))} {
		msg := baseMessage()
		msg.Subject = value
		msg.From.Name = value
		msg.To[0].Name = value
		encoded := encode(t, msg)
		parsed := parse(t, encoded)

		subject := parsed.Header.Get("Subject")
		if strings.Contains(subject, "QmNjOiBldmlsQGV4YW1wbGUubmV0") && !strings.HasPrefix(subject, "=?utf-8?b?") {
			t.Errorf("literal encoded-word left in the subject: %q", subject)
		}
		decoded, err := new(mime.WordDecoder).DecodeHeader(subject)
		if err != nil || decoded != strings.TrimSpace(value) {
			t.Errorf("subject decodes to %q (%v), want the literal %q", decoded, err, value)
		}
		from, err := parsed.Header.AddressList("From")
		if err != nil || from[0].Name != value {
			t.Errorf("From name decodes to %v (%v), want %q", from, err, value)
		}
		to, err := parsed.Header.AddressList("To")
		if err != nil || to[0].Name != value {
			t.Errorf("To name decodes to %v (%v), want %q", to, err, value)
		}
		assertCleanHeaderLines(t, encoded.Data)
	}
}
