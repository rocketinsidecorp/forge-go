package mail_test

import (
	"errors"
	"strings"
	"testing"

	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

// Every limit can be tightened below what the defaults allow, and a message
// the defaults accept is then refused; the other limits keep their defaults.
func TestEachConfiguredLimitIsEnforced(t *testing.T) {
	t.Parallel()
	attach := func(sizes ...int) func(*forgemail.Message) {
		return func(m *forgemail.Message) {
			for i, size := range sizes {
				m.Attachments = append(m.Attachments, forgemail.Attachment{Filename: "f" + string(rune('a'+i)) + ".bin", Data: make([]byte, size)})
			}
		}
	}
	cases := map[string]struct {
		limits forgemail.Limits
		mutate func(*forgemail.Message)
	}{
		"subject":          {forgemail.Limits{MaxSubjectRunes: 5}, func(m *forgemail.Message) { m.Subject = "longer than five" }},
		"recipients":       {forgemail.Limits{MaxRecipients: 1}, func(m *forgemail.Message) { m.Cc = []forgemail.Address{{Email: "bob@example.org"}} }},
		"body":             {forgemail.Limits{MaxBodyBytes: 10}, func(m *forgemail.Message) { m.Text = strings.Repeat("x", 11) }},
		"attachments":      {forgemail.Limits{MaxAttachments: 1}, attach(1, 1)},
		"attachment bytes": {forgemail.Limits{MaxAttachmentBytes: 5}, attach(6)},
		"message bytes":    {forgemail.Limits{MaxMessageBytes: 100}, attach(101)},
	}
	for name, tc := range cases {
		msg := baseMessage()
		tc.mutate(&msg)
		if _, err := forgemail.Encode(msg); err != nil {
			t.Errorf("%s: the defaults refuse the control message: %v", name, err)
			continue
		}
		if _, err := forgemail.Encode(msg, forgemail.WithLimits(tc.limits)); !errors.Is(err, forgemail.ErrInvalid) {
			t.Errorf("%s: a tightened limit accepted the message: %v", name, err)
		}
	}
}

func TestAddressesHeadersAndContentIDsAreBounded(t *testing.T) {
	t.Parallel()
	tooManyReplyTo := baseMessage()
	for i := range 11 {
		tooManyReplyTo.ReplyTo = append(tooManyReplyTo.ReplyTo, forgemail.Address{Email: "r" + string(rune('a'+i)) + "@example.org"})
	}
	emptyHeader := baseMessage()
	emptyHeader.Headers = map[string]string{"List-Unsubscribe": ""}
	longHeader := baseMessage()
	longHeader.Headers = map[string]string{"List-Unsubscribe": "<" + strings.Repeat("a", 1000) + ">"}
	longContentID := baseMessage()
	longContentID.HTML = "<p>x</p>"
	longContentID.Attachments = []forgemail.Attachment{{Filename: "a.png", ContentID: strings.Repeat("a", 129), Data: []byte("x")}}
	badContentID := baseMessage()
	badContentID.HTML = "<p>x</p>"
	badContentID.Attachments = []forgemail.Attachment{{Filename: "a.png", ContentID: "has space", Data: []byte("x")}}
	for name, msg := range map[string]forgemail.Message{
		"eleven reply-to addresses": tooManyReplyTo,
		"an empty header value":     emptyHeader,
		"an oversized header value": longHeader,
		"a content id over 128":     longContentID,
		"a content id with a space": badContentID,
	} {
		if _, err := forgemail.Encode(msg); !errors.Is(err, forgemail.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}
