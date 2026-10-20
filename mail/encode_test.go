package mail_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"testing"
	"time"

	forgemail "github.com/rocketinsidecorp/forge-go/mail"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var fixedNow = time.Date(2026, 10, 14, 15, 4, 5, 0, time.FixedZone("BRT", -3*3600))

func baseMessage() forgemail.Message {
	return forgemail.Message{
		From:    forgemail.Address{Name: "Forge App", Email: "app@example.com"},
		To:      []forgemail.Address{{Name: "Alice", Email: "alice@example.org"}},
		Subject: "Welcome",
		Text:    "Hello Alice\nSecond line",
	}
}

func encode(t *testing.T, msg forgemail.Message) forgemail.Encoded {
	t.Helper()
	encoded, err := forgemail.Encode(msg, forgemail.WithClock(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return encoded
}

func parse(t *testing.T, encoded forgemail.Encoded) *mail.Message {
	t.Helper()
	parsed, err := mail.ReadMessage(bytes.NewReader(encoded.Data))
	if err != nil {
		t.Fatalf("net/mail cannot parse the output: %v\n%s", err, encoded.Data)
	}
	return parsed
}

func decodeBody(t *testing.T, cte string, r io.Reader) string {
	t.Helper()
	switch strings.ToLower(cte) {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(decodeBase64(t, string(data)))
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestEncodeTextOnly(t *testing.T) {
	t.Parallel()
	encoded := encode(t, baseMessage())
	parsed := parse(t, encoded)

	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/plain" || params["charset"] != "utf-8" {
		t.Fatalf("content type = %q %v %v", mediaType, params, err)
	}
	if got := decodeBody(t, parsed.Header.Get("Content-Transfer-Encoding"), parsed.Body); strings.ReplaceAll(got, "\r\n", "\n") != "Hello Alice\nSecond line\n" {
		t.Fatalf("body = %q", got)
	}
	if got := parsed.Header.Get("Date"); got != "Wed, 14 Oct 2026 18:04:05 +0000" {
		t.Fatalf("Date = %q, want UTC", got)
	}
	date, err := parsed.Header.Date()
	if err != nil || !date.Equal(fixedNow) {
		t.Fatalf("Date parse = %v, %v", date, err)
	}
	if parsed.Header.Get("MIME-Version") != "1.0" {
		t.Fatal("missing MIME-Version")
	}
	from, err := parsed.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Name != "Forge App" || from[0].Address != "app@example.com" {
		t.Fatalf("From = %v, %v", from, err)
	}
	if encoded.Sender != "app@example.com" || len(encoded.Recipients) != 1 {
		t.Fatalf("envelope = %+v", encoded)
	}
}

var messageIDPattern = regexp.MustCompile(`^<([0-9a-f-]{36})@example\.com>$`)

func TestMessageIDIsUUIDv7AtSenderDomain(t *testing.T) {
	t.Parallel()
	msg := baseMessage()
	msg.From.Email = "App@Example.COM"
	first, second := encode(t, msg), encode(t, msg)
	match := messageIDPattern.FindStringSubmatch(first.MessageID)
	if match == nil {
		t.Fatalf("Message-ID %q does not look like <uuid@example.com>", first.MessageID)
	}
	id, err := uuid.Parse(match[1])
	if err != nil {
		t.Fatalf("Message-ID is not a UUIDv7: %v", err)
	}
	if id.Version() != 7 {
		t.Fatalf("version = %d", id.Version())
	}
	if first.MessageID == second.MessageID {
		t.Fatal("Message-ID repeated")
	}
	if got := parse(t, first).Header.Get("Message-ID"); got != first.MessageID {
		t.Fatalf("header Message-ID = %q", got)
	}
}

func TestEncodeAlternativeStructure(t *testing.T) {
	t.Parallel()
	msg := baseMessage()
	msg.HTML = "<p>Hello <b>Alice</b> — café</p>"
	msg.Text = "Hello Alice — café"
	parsed := parse(t, encode(t, msg))

	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("content type = %q %v", mediaType, err)
	}
	reader := multipart.NewReader(parsed.Body, params["boundary"])
	var types, bodies []string
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		partType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		types = append(types, partType)
		bodies = append(bodies, strings.TrimSpace(decodeBody(t, part.Header.Get("Content-Transfer-Encoding"), part)))
	}
	if strings.Join(types, ",") != "text/plain,text/html" {
		t.Fatalf("parts = %v (html must come last: clients prefer the last alternative)", types)
	}
	if bodies[0] != msg.Text || bodies[1] != msg.HTML {
		t.Fatalf("bodies = %q", bodies)
	}
}

func TestEncodeMixedRelatedStructureAndAttachments(t *testing.T) {
	t.Parallel()
	pdf := bytes.Repeat([]byte("%PDF-1.7 binary \x00\xff"), 200)
	png := []byte("\x89PNG\r\n\x1a\nfake")
	msg := baseMessage()
	msg.HTML = `<img src="cid:logo@forge">`
	msg.Attachments = []forgemail.Attachment{
		{Filename: "relatório final.pdf", ContentType: "application/pdf", Data: pdf},
		{Filename: "logo.png", ContentType: "image/png", Data: png, ContentID: "logo@forge"},
	}
	parsed := parse(t, encode(t, msg))

	mediaType, params, _ := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if mediaType != "multipart/mixed" {
		t.Fatalf("root = %q", mediaType)
	}
	root := multipart.NewReader(parsed.Body, params["boundary"])

	first, err := root.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	alternativeType, alternativeParams, _ := mime.ParseMediaType(first.Header.Get("Content-Type"))
	if alternativeType != "multipart/alternative" {
		t.Fatalf("first = %q", alternativeType)
	}
	alternative := multipart.NewReader(first, alternativeParams["boundary"])
	if _, err := alternative.NextPart(); err != nil { // text/plain
		t.Fatal(err)
	}
	related, err := alternative.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	relatedType, relatedParams, _ := mime.ParseMediaType(related.Header.Get("Content-Type"))
	if relatedType != "multipart/related" {
		t.Fatalf("html part = %q, want multipart/related", relatedType)
	}
	relatedReader := multipart.NewReader(related, relatedParams["boundary"])
	htmlPart, err := relatedReader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(decodeBody(t, htmlPart.Header.Get("Content-Transfer-Encoding"), htmlPart)); got != msg.HTML {
		t.Fatalf("html = %q", got)
	}
	inline, err := relatedReader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if inline.Header.Get("Content-Id") != "<logo@forge>" || !strings.HasPrefix(inline.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("inline headers = %v", inline.Header)
	}
	if got := decodeBody(t, "base64", inline); got != string(png) {
		t.Fatalf("inline data = %q", got)
	}

	file, err := root.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	disposition, dispositionParams, err := mime.ParseMediaType(file.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || dispositionParams["filename"] != "relatório final.pdf" {
		t.Fatalf("disposition = %q %v %v", disposition, dispositionParams, err)
	}
	if got := decodeBody(t, "base64", file); got != string(pdf) {
		t.Fatal("attachment bytes changed")
	}
	if _, err := root.NextPart(); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected extra part: %v", err)
	}
}

func TestAttachmentBase64LinesAreWrapped(t *testing.T) {
	t.Parallel()
	msg := baseMessage()
	msg.Attachments = []forgemail.Attachment{{Filename: "a.bin", Data: bytes.Repeat([]byte{0xab}, 5000)}}
	encoded := encode(t, msg)
	for _, line := range strings.Split(string(encoded.Data), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line of %d bytes exceeds RFC 5322 limit", len(line))
		}
	}
	if bytes.Contains(encoded.Data, []byte("\n\n")) || bytes.Contains(bytes.ReplaceAll(encoded.Data, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatal("output contains a bare LF")
	}
	for _, b := range encoded.Data {
		if b > 0x7e {
			t.Fatalf("output is not 7-bit clean (byte %#x)", b)
		}
	}
}

func TestBccIsEnvelopeOnly(t *testing.T) {
	t.Parallel()
	msg := baseMessage()
	msg.Cc = []forgemail.Address{{Email: "cc@example.org"}}
	msg.Bcc = []forgemail.Address{{Email: "secret@example.net"}, {Email: "ALICE@example.org"}}
	msg.ReplyTo = []forgemail.Address{{Name: "Support", Email: "support@example.com"}}
	encoded := encode(t, msg)
	// Look for a Bcc header field, not the letters "bcc": the random hex MIME
	// boundary and the UUIDv7 Message-ID contain them about once in a
	// hundred messages.
	lower := bytes.ToLower(encoded.Data)
	if bytes.Contains(lower, []byte("secret@example.net")) || bytes.HasPrefix(lower, []byte("bcc:")) || bytes.Contains(lower, []byte("\r\nbcc:")) {
		t.Fatal("Bcc recipient leaked into the message")
	}
	want := []string{"alice@example.org", "cc@example.org", "secret@example.net"}
	if strings.Join(encoded.Recipients, ",") != strings.Join(want, ",") {
		t.Fatalf("recipients = %v, want %v (de-duplicated case-insensitively)", encoded.Recipients, want)
	}
	parsed := parse(t, encoded)
	if reply, err := parsed.Header.AddressList("Reply-To"); err != nil || len(reply) != 1 || reply[0].Address != "support@example.com" {
		t.Fatalf("Reply-To = %v %v", reply, err)
	}
}

func TestNonASCIINamesAndSubjectsRoundTrip(t *testing.T) {
	t.Parallel()
	subject := "Olá, João! Seu pedido nº 42 — 日本語 ✓ " + strings.Repeat("ação ", 30)
	msg := baseMessage()
	msg.From.Name = "José da Silva (Forge) \"Ltda\""
	msg.To = []forgemail.Address{{Name: "Zoë, Müller", Email: "zoe@example.org"}, {Name: "李雷", Email: "li@example.org"}}
	msg.Subject = subject
	msg.Text = "Olá — corpo com acentuação\n"
	encoded := encode(t, msg)

	for _, line := range strings.Split(string(encoded.Data), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line too long: %d", len(line))
		}
	}
	headerEnd := bytes.Index(encoded.Data, []byte("\r\n\r\n"))
	for _, line := range strings.Split(string(encoded.Data[:headerEnd]), "\r\n") {
		if len(line) > 78 && !strings.Contains(line, "filename") {
			t.Fatalf("header line of %d columns was not folded: %q", len(line), line)
		}
	}
	parsed := parse(t, encoded)
	decoder := new(mime.WordDecoder)
	got, err := decoder.DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil || got != subject {
		t.Fatalf("subject = %q (%v), want %q", got, err, subject)
	}
	from, err := parsed.Header.AddressList("From")
	if err != nil || from[0].Name != msg.From.Name {
		t.Fatalf("From = %+v %v", from, err)
	}
	to, err := parsed.Header.AddressList("To")
	if err != nil || len(to) != 2 || to[0].Name != "Zoë, Müller" || to[1].Name != "李雷" {
		t.Fatalf("To = %+v %v", to, err)
	}
	if got := decodeBody(t, parsed.Header.Get("Content-Transfer-Encoding"), parsed.Body); !strings.Contains(got, "Olá — corpo com acentuação") {
		t.Fatalf("body = %q", got)
	}
}

func TestBodyLineEndingsAreNormalised(t *testing.T) {
	t.Parallel()
	msg := baseMessage()
	msg.Text = "one\rtwo\r\nthree\nfour " + strings.Repeat("x", 200) + "\n.dot\nFrom me"
	encoded := encode(t, msg)
	parsed := parse(t, encoded)
	got := strings.ReplaceAll(decodeBody(t, parsed.Header.Get("Content-Transfer-Encoding"), parsed.Body), "\r\n", "\n")
	want := "one\ntwo\nthree\nfour " + strings.Repeat("x", 200) + "\n.dot\nFrom me\n"
	if got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if regexp.MustCompile(`[^\r]\n`).Match(encoded.Data) {
		t.Fatalf("bare LF in output: %q", encoded.Data)
	}
}

func TestExtraHeadersAreAllowlisted(t *testing.T) {
	t.Parallel()
	msg := baseMessage()
	msg.Headers = map[string]string{
		"list-unsubscribe":      "<https://example.com/u/1>, <mailto:u@example.com>",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		"Auto-Submitted":        "auto-generated",
	}
	parsed := parse(t, encode(t, msg))
	if got := parsed.Header.Get("List-Unsubscribe"); got != "<https://example.com/u/1>, <mailto:u@example.com>" {
		t.Fatalf("List-Unsubscribe = %q", got)
	}
	if parsed.Header.Get("Auto-Submitted") != "auto-generated" {
		t.Fatal("Auto-Submitted missing")
	}
	for _, name := range []string{"Bcc", "To", "Subject", "Content-Type", "Content-Transfer-Encoding", "X-Anything", "From", "Return-Path", "Message-ID"} {
		msg := baseMessage()
		msg.Headers = map[string]string{name: "x"}
		if _, err := forgemail.Encode(msg); !errors.Is(err, forgemail.ErrInvalid) {
			t.Errorf("header %q accepted: %v", name, err)
		}
	}
	msg.Headers = map[string]string{"List-Id": "a", "list-id": "b"}
	if _, err := forgemail.Encode(msg); !errors.Is(err, forgemail.ErrInvalid) {
		t.Error("case-variant duplicate header accepted")
	}
	if !contains(forgemail.AllowedHeaders(), "List-Unsubscribe") {
		t.Fatal("AllowedHeaders misses List-Unsubscribe")
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// injections are the payloads every user-controlled field must refuse.
var injections = []string{
	"x\r\nBcc: evil@example.net",
	"x\nBcc: evil@example.net",
	"x\rBcc: evil@example.net",
	"x\r\n\r\nbody smuggled",
	"x\x00y",
	"x\u2028Bcc: evil@example.net",
	"x\u2029y",
	"x\x7fy",
	"x\u0085y",
}

func TestHeaderInjectionIsRejectedEverywhere(t *testing.T) {
	t.Parallel()
	for _, payload := range injections {
		fields := map[string]func(*forgemail.Message){
			"Subject":         func(m *forgemail.Message) { m.Subject = payload },
			"From.Name":       func(m *forgemail.Message) { m.From.Name = payload },
			"From.Email":      func(m *forgemail.Message) { m.From.Email = "a@example.com" + payload },
			"From.Email.user": func(m *forgemail.Message) { m.From.Email = payload + "@example.com" },
			"To.Name":         func(m *forgemail.Message) { m.To[0].Name = payload },
			"To.Email":        func(m *forgemail.Message) { m.To[0].Email = "a@example.org" + payload },
			"Cc.Name":         func(m *forgemail.Message) { m.Cc = []forgemail.Address{{Name: payload, Email: "c@example.org"}} },
			"Cc.Email":        func(m *forgemail.Message) { m.Cc = []forgemail.Address{{Email: "c@example.org" + payload}} },
			"Bcc.Name":        func(m *forgemail.Message) { m.Bcc = []forgemail.Address{{Name: payload, Email: "c@example.org"}} },
			"Bcc.Email":       func(m *forgemail.Message) { m.Bcc = []forgemail.Address{{Email: "c@example.org" + payload}} },
			"ReplyTo.Name":    func(m *forgemail.Message) { m.ReplyTo = []forgemail.Address{{Name: payload, Email: "r@example.org"}} },
			"ReplyTo.Email":   func(m *forgemail.Message) { m.ReplyTo = []forgemail.Address{{Email: "r@example.org" + payload}} },
			"Header value":    func(m *forgemail.Message) { m.Headers = map[string]string{"List-Id": payload} },
			"Header name":     func(m *forgemail.Message) { m.Headers = map[string]string{"List-Id" + payload: "x"} },
			"Filename": func(m *forgemail.Message) {
				m.Attachments = []forgemail.Attachment{{Filename: "a" + payload + ".txt", Data: []byte("x")}}
			},
			"ContentType": func(m *forgemail.Message) {
				m.Attachments = []forgemail.Attachment{{Filename: "a.txt", ContentType: "text/plain" + payload, Data: []byte("x")}}
			},
			"ContentID": func(m *forgemail.Message) {
				m.HTML = "<p>x</p>"
				m.Attachments = []forgemail.Attachment{{Filename: "a.png", ContentID: "id" + payload, Data: []byte("x")}}
			},
		}
		for name, mutate := range fields {
			msg := baseMessage()
			mutate(&msg)
			encoded, err := forgemail.Encode(msg)
			if err == nil {
				t.Errorf("%s accepted payload %q:\n%s", name, payload, encoded.Data)
				continue
			}
			if !errors.Is(err, forgemail.ErrInvalid) {
				t.Errorf("%s: error %v is not ErrInvalid", name, err)
			}
			if strings.ContainsAny(err.Error(), "\r\n\x00") || strings.Contains(err.Error(), "evil") {
				t.Errorf("%s: error echoes the payload: %q", name, err.Error())
			}
		}
	}
}

func TestAddressStrictness(t *testing.T) {
	t.Parallel()
	bad := []string{
		"", "plain", "a@", "@b.com", "a@b@c.com", "a b@example.com", "<a@example.com>", "Name <a@example.com>",
		`"quoted local"@example.com`, `"a@b"@example.com`, "a@[127.0.0.1]", "a@exa_mple.com", "a@-example.com", "a@example..com",
		"a..b@example.com", ".a@example.com", "a.@example.com", "ação@example.com", "a@exämple.com", "a@example.com,b@example.org",
		"a@example.com;b@example.org", "a(comment)@example.com", "a@example.com>", "a@b.com\t", strings.Repeat("a", 65) + "@example.com",
		"a@" + strings.Repeat("a", 64) + ".com", "a@" + strings.Repeat("a.", 130) + "com",
	}
	for _, email := range bad {
		if _, err := forgemail.NewAddress("", email); err == nil {
			t.Errorf("NewAddress accepted %q", email)
		}
	}
	good := []string{"a@example.com", "first.last+tag@sub.example.co.uk", "o'brien@example.com", "a@localhost", "x_y@example.com", "A@Example.COM"}
	for _, email := range good {
		if _, err := forgemail.NewAddress("", email); err != nil {
			t.Errorf("NewAddress rejected %q: %v", email, err)
		}
	}
	parsed, err := forgemail.ParseAddress("João Silva <joao@example.com>")
	if err != nil || parsed.Name != "João Silva" || parsed.Email != "joao@example.com" {
		t.Fatalf("ParseAddress = %+v %v", parsed, err)
	}
	parsed, err = forgemail.ParseAddress("=?utf-8?q?Jo=C3=A3o?= <joao@example.com>")
	if err != nil || parsed.Name != "João" {
		t.Fatalf("ParseAddress(encoded) = %+v %v", parsed, err)
	}
	for _, value := range []string{"a@example.com, b@example.com", "Name <a@example.com>\r\nBcc: x@y.com", "garbage", ""} {
		if _, err := forgemail.ParseAddress(value); err == nil {
			t.Errorf("ParseAddress accepted %q", value)
		}
	}
}

func TestRequiredFieldsAndLimits(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*forgemail.Message){
		"no recipient":     func(m *forgemail.Message) { m.To = nil },
		"only empty":       func(m *forgemail.Message) { m.To = []forgemail.Address{{}} },
		"no sender":        func(m *forgemail.Message) { m.From = forgemail.Address{} },
		"no body":          func(m *forgemail.Message) { m.Text = "" },
		"subject too long": func(m *forgemail.Message) { m.Subject = strings.Repeat("é", 256) },
		"too many recipients": func(m *forgemail.Message) {
			for i := 0; i < 101; i++ {
				m.Bcc = append(m.Bcc, forgemail.Address{Email: "u" + strings.Repeat("a", i%5) + string(rune('a'+i%26)) + "@example.org"})
			}
		},
		"name too long":     func(m *forgemail.Message) { m.From.Name = strings.Repeat("n", 256) },
		"body too large":    func(m *forgemail.Message) { m.Text = strings.Repeat("x", 2<<20+1) },
		"invalid utf8 body": func(m *forgemail.Message) { m.Text = "a\xffb" },
		"empty attachment": func(m *forgemail.Message) {
			m.Attachments = []forgemail.Attachment{{Filename: "a.txt"}}
		},
		"attachment too large": func(m *forgemail.Message) {
			m.Attachments = []forgemail.Attachment{{Filename: "a.bin", Data: make([]byte, 7<<20+1)}}
		},
		"total too large": func(m *forgemail.Message) {
			m.Attachments = []forgemail.Attachment{
				{Filename: "a.bin", Data: make([]byte, 6<<20)},
				{Filename: "b.bin", Data: make([]byte, 6<<20)},
			}
		},
		"too many attachments": func(m *forgemail.Message) {
			for i := 0; i < 11; i++ {
				m.Attachments = append(m.Attachments, forgemail.Attachment{Filename: "a.txt", Data: []byte("x")})
			}
		},
		"multipart attachment type": func(m *forgemail.Message) {
			m.Attachments = []forgemail.Attachment{{Filename: "a", ContentType: "multipart/mixed", Data: []byte("x")}}
		},
		"inline without html": func(m *forgemail.Message) {
			m.Attachments = []forgemail.Attachment{{Filename: "a.png", ContentID: "x", Data: []byte("x")}}
		},
		"duplicate content id": func(m *forgemail.Message) {
			m.HTML = "<p>x</p>"
			m.Attachments = []forgemail.Attachment{{Filename: "a.png", ContentID: "x", Data: []byte("x")}, {Filename: "b.png", ContentID: "x", Data: []byte("x")}}
		},
	}
	for name, mutate := range cases {
		msg := baseMessage()
		mutate(&msg)
		if _, err := forgemail.Encode(msg); !errors.Is(err, forgemail.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	msg := baseMessage()
	msg.Subject = strings.Repeat("é", 300)
	if _, err := forgemail.Encode(msg, forgemail.WithLimits(forgemail.Limits{MaxSubjectRunes: 300})); err != nil {
		t.Errorf("custom limit not honoured: %v", err)
	}
	msg = baseMessage()
	msg.Subject = ""
	if _, err := forgemail.Encode(msg); err != nil {
		t.Errorf("empty subject must be allowed: %v", err)
	}
}

func TestSanitizeFilename(t *testing.T) {
	t.Parallel()
	good := map[string]string{
		"report.pdf":              "report.pdf",
		"../../etc/passwd":        "passwd",
		`C:\Users\x\invoice.pdf`:  "invoice.pdf",
		"  spaced name.txt  ":     "spaced name.txt",
		".hidden":                 "hidden",
		"dir/":                    "dir",
		"relatório — final.pdf":   "relatório — final.pdf",
		`quote".txt`:              `quote".txt`,
		"semi;colon=equals.txt":   "semi;colon=equals.txt",
		strings.Repeat("a", 255):  strings.Repeat("a", 255),
		"nested/path/to/file.zip": "file.zip",
	}
	for in, want := range good {
		got, err := forgemail.SanitizeFilename(in)
		if err != nil || got != want {
			t.Errorf("SanitizeFilename(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "..", "/", ".", " . ", "a\x00b", "a\nb", "invoice\u202efdp.exe", "x\u2066y", strings.Repeat("a", 256), "a\xffb", "a\u200fb"} {
		if got, err := forgemail.SanitizeFilename(in); err == nil {
			t.Errorf("SanitizeFilename(%q) = %q, want error", in, got)
		}
	}
}

func TestSanitizedAttachmentHeadersCannotBeBroken(t *testing.T) {
	t.Parallel()
	msg := baseMessage()
	msg.Attachments = []forgemail.Attachment{{Filename: `evil"; filename="x.exe`, Data: []byte("x")}}
	parsed := parse(t, encode(t, msg))
	_, params, _ := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	reader := multipart.NewReader(parsed.Body, params["boundary"])
	if _, err := reader.NextPart(); err != nil { // the text body
		t.Fatal(err)
	}
	part, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	_, p, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil || p["filename"] != `evil"; filename="x.exe` {
		t.Fatalf("filename = %q (%v): quotes must stay inside the parameter", p["filename"], err)
	}
}

func TestDeterministicOptions(t *testing.T) {
	t.Parallel()
	generator, err := uuid.NewGenerator(func() time.Time { return fixedNow }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	msg := baseMessage()
	msg.HTML = "<p>x</p>"
	encoded, err := forgemail.Encode(msg, forgemail.WithClock(func() time.Time { return fixedNow }), forgemail.WithIDGenerator(generator), forgemail.WithBoundaryEntropy(bytes.NewReader(make([]byte, 64))))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded.Data, []byte("forge-00000000000000000000000000000000")) {
		t.Fatalf("boundary entropy not used:\n%s", encoded.Data)
	}
	if _, err := forgemail.Encode(msg, forgemail.WithBoundaryEntropy(bytes.NewReader(nil))); err == nil {
		t.Fatal("entropy failure must fail the encode")
	}
}

func TestClonePreventsAliasing(t *testing.T) {
	t.Parallel()
	msg := baseMessage()
	msg.Headers = map[string]string{"List-Id": "a"}
	msg.Attachments = []forgemail.Attachment{{Filename: "a", Data: []byte("abc")}}
	clone := msg.Clone()
	clone.To[0].Email = "changed@example.org"
	clone.Headers["List-Id"] = "b"
	clone.Attachments[0].Data[0] = 'z'
	if msg.To[0].Email == "changed@example.org" || msg.Headers["List-Id"] != "a" || msg.Attachments[0].Data[0] != 'a' {
		t.Fatal("Clone shares memory with the original")
	}
}

func FuzzAddressAndHeaderValidation(f *testing.F) {
	for _, seed := range []string{"a@example.com", "Name <a@example.com>", "x\r\nBcc: y@z.com", "ação@é.com", `"a b"@c.com`, "a@[1.2.3.4]", "\x00", "a@b\u2028c"} {
		f.Add(seed, seed, seed)
	}
	f.Fuzz(func(t *testing.T, name, email, subject string) {
		msg := forgemail.Message{
			From:    forgemail.Address{Name: name, Email: "sender@example.com"},
			To:      []forgemail.Address{{Name: name, Email: email}},
			Subject: subject,
			Text:    "body",
			Headers: map[string]string{"List-Id": subject},
		}
		encoded, err := forgemail.Encode(msg)
		if err != nil {
			if !errors.Is(err, forgemail.ErrInvalid) && !errors.Is(err, forgemail.ErrAddressUnsupported) {
				t.Fatalf("unexpected error type: %v", err)
			}
			return
		}
		// Whatever was accepted must re-parse as exactly the intended
		// message: one To address, no extra headers, the envelope matching.
		parsed, err := mail.ReadMessage(bytes.NewReader(encoded.Data))
		if err != nil {
			t.Fatalf("accepted message does not parse: %v", err)
		}
		if _, ok := parsed.Header["Bcc"]; ok {
			t.Fatal("injected Bcc header")
		}
		to, err := parsed.Header.AddressList("To")
		if err != nil || len(to) != 1 || to[0].Address != email {
			t.Fatalf("To = %v, %v for %q", to, err, email)
		}
		if len(encoded.Recipients) != 1 || encoded.Recipients[0] != email {
			t.Fatalf("envelope = %v for %q", encoded.Recipients, email)
		}
		for _, line := range strings.Split(string(encoded.Data), "\r\n") {
			if len(line) > 998 {
				t.Fatalf("line of %d bytes", len(line))
			}
		}
		decoded, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
		// Surrounding whitespace is not significant in a header value.
		if err != nil || decoded != strings.TrimSpace(subject) {
			t.Fatalf("subject %q round-tripped to %q (%v)", subject, decoded, err)
		}
		for key := range parsed.Header {
			switch key {
			case "Date", "From", "To", "Subject", "Message-Id", "Mime-Version", "List-Id", "Content-Type", "Content-Transfer-Encoding":
			default:
				t.Fatalf("unexpected header %q", key)
			}
		}
	})
}

func decodeBase64(t *testing.T, text string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\r", "", "\n", "").Replace(text))
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	return data
}
