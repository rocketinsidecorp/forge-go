package mail_test

import (
	"errors"
	"testing"

	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

// Direction overrides in a display name or subject let an attacker reorder
// what the recipient reads (a sender shown as "Bank" that is really another
// string), so they are rejected like control characters.
func TestBidiControlsAreRejectedInDisplayNamesAndSubjects(t *testing.T) {
	t.Parallel()
	var runes []rune
	for r := '\u202a'; r <= '\u202e'; r++ {
		runes = append(runes, r)
	}
	for r := '\u2066'; r <= '\u2069'; r++ {
		runes = append(runes, r)
	}
	runes = append(runes, '\u200e', '\u200f', '\u061c')
	for _, r := range runes {
		payload := "Pay" + string(r) + "pal"
		cases := map[string]func(*forgemail.Message){
			"Subject":      func(m *forgemail.Message) { m.Subject = payload },
			"From.Name":    func(m *forgemail.Message) { m.From.Name = payload },
			"To.Name":      func(m *forgemail.Message) { m.To[0].Name = payload },
			"Cc.Name":      func(m *forgemail.Message) { m.Cc = []forgemail.Address{{Name: payload, Email: "c@example.org"}} },
			"ReplyTo.Name": func(m *forgemail.Message) { m.ReplyTo = []forgemail.Address{{Name: payload, Email: "r@example.org"}} },
		}
		for name, mutate := range cases {
			msg := baseMessage()
			mutate(&msg)
			if _, err := forgemail.Encode(msg); !errors.Is(err, forgemail.ErrInvalid) {
				t.Errorf("%s accepted U+%04X: %v", name, r, err)
			}
		}
		if _, err := forgemail.NewAddress(payload, "a@example.com"); err == nil {
			t.Errorf("NewAddress accepted U+%04X", r)
		}
	}
	// Ordinary right-to-left text without overrides is still fine.
	msg := baseMessage()
	msg.Subject = "שלום עולם مرحبا"
	msg.From.Name = "مرحبا"
	if _, err := forgemail.Encode(msg); err != nil {
		t.Fatalf("plain RTL text rejected: %v", err)
	}
}
