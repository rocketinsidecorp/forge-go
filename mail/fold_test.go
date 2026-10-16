package mail_test

import (
	"bytes"
	mathrand "math/rand"
	"regexp"
	"strings"
	"testing"

	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

// A header line made only of whitespace, or ending in whitespace, is stripped
// by some gateways; a stripped whitespace-only line becomes the blank line
// that ends the header block, turning the rest of the headers into body.
func assertCleanHeaderLines(t *testing.T, data []byte) {
	t.Helper()
	end := bytes.Index(data, []byte("\r\n\r\n"))
	if end < 0 {
		t.Fatal("no header terminator")
	}
	blank := regexp.MustCompile(`^[ \t]*$`)
	for _, line := range strings.Split(string(data[:end]), "\r\n") {
		if blank.MatchString(line) {
			t.Fatalf("whitespace-only header line %q in:\n%s", line, data[:end])
		}
		if strings.HasSuffix(line, " ") || strings.HasSuffix(line, "\t") {
			t.Fatalf("header line with trailing whitespace %q in:\n%s", line, data[:end])
		}
	}
}

func TestFoldedHeadersNeverHaveBlankOrTrailingWhitespaceLines(t *testing.T) {
	t.Parallel()
	word := func(n int) string { return strings.Repeat("a", n) }
	values := []string{
		"a" + strings.Repeat(" ", 100) + "b",
		strings.Repeat(word(10)+strings.Repeat(" ", 7), 14),
		word(75) + "  " + word(10),
		word(70) + strings.Repeat(" ", 30) + word(70),
		"   leading and trailing   ",
		strings.Repeat(" ", 50),
		"",
		strings.Repeat("ab  ", 200)[:255],
		"ç" + strings.Repeat(" ", 80) + "ã",
	}
	for _, value := range values {
		msg := baseMessage()
		msg.Subject = value
		msg.Headers = map[string]string{"References": strings.Repeat("<id@example.com>"+strings.Repeat(" ", 6), 41)[:890] + " x"}
		msg.From.Name = strings.Repeat("N"+strings.Repeat(" ", 9), 20)
		assertCleanHeaderLines(t, encode(t, msg).Data)
	}
	// Property check over random runs of words and spaces.
	rng := mathrand.New(mathrand.NewSource(7))
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for b.Len() < 240 {
			b.WriteString(strings.Repeat(" ", rng.Intn(9)))
			b.WriteString(strings.Repeat("x", 1+rng.Intn(40)))
		}
		value := b.String()[:240]
		msg := baseMessage()
		msg.Subject = value
		msg.Headers = map[string]string{"References": value}
		data := encode(t, msg).Data
		assertCleanHeaderLines(t, data)
		parsed := parse(t, forgemail.Encoded{Data: data})
		// net/mail joins continuation lines with one space, so compare the
		// words: folding must neither drop nor add any.
		got, want := strings.Fields(parsed.Header.Get("References")), strings.Fields(value)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("folding changed the value: %q != %q", got, want)
		}
	}
}
