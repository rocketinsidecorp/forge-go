package mail_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	forgemail "github.com/rocketinsidecorp/forge-go/mail"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

type failingEntropy struct{}

func (failingEntropy) Read([]byte) (int, error) { return 0, errors.New("entropy exhausted") }

func TestEncodeHTMLOnlyWithInlineImageIsMultipartRelated(t *testing.T) {
	t.Parallel()
	msg := baseMessage()
	msg.Text = ""
	msg.HTML = `<img src="cid:logo@forge">`
	msg.Attachments = []forgemail.Attachment{{Filename: "logo.png", ContentType: "image/png", Data: []byte("\x89PNG"), ContentID: "logo@forge"}}
	parsed := parse(t, encode(t, msg))
	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/related" {
		t.Fatalf("root = %q, %v", mediaType, err)
	}
	reader := multipart.NewReader(parsed.Body, params["boundary"])
	html, err := reader.NextPart()
	if err != nil || !strings.HasPrefix(html.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("first part = %v, %v", html.Header, err)
	}
	image, err := reader.NextPart()
	if err != nil || image.Header.Get("Content-Id") != "<logo@forge>" || !strings.HasPrefix(image.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("second part = %v, %v", image.Header, err)
	}
	if _, err := io.ReadAll(image); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeReportsEntropyFailures(t *testing.T) {
	t.Parallel()
	// A multipart message needs a boundary.
	msg := baseMessage()
	msg.HTML = "<p>hi</p>"
	if _, err := forgemail.Encode(msg, forgemail.WithBoundaryEntropy(failingEntropy{})); err == nil || !strings.Contains(err.Error(), "boundary") {
		t.Errorf("boundary: %v", err)
	}
	mixed := baseMessage()
	mixed.Attachments = []forgemail.Attachment{{Filename: "a.txt", ContentType: "text/plain", Data: []byte("x")}}
	if _, err := forgemail.Encode(mixed, forgemail.WithBoundaryEntropy(failingEntropy{})); err == nil {
		t.Error("an attachment message encoded without boundary entropy")
	}
	related := baseMessage()
	related.Text = ""
	related.HTML = `<img src="cid:a">`
	related.Attachments = []forgemail.Attachment{{Filename: "a.png", ContentType: "image/png", Data: []byte("x"), ContentID: "a"}}
	if _, err := forgemail.Encode(related, forgemail.WithBoundaryEntropy(failingEntropy{})); err == nil {
		t.Error("an inline message encoded without boundary entropy")
	}
	// A plain text message needs none.
	if _, err := forgemail.Encode(baseMessage(), forgemail.WithBoundaryEntropy(failingEntropy{})); err != nil {
		t.Errorf("plain text: %v", err)
	}
	// The message ID needs entropy too.
	generator, err := uuid.NewGenerator(time.Now, failingEntropy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := forgemail.Encode(baseMessage(), forgemail.WithIDGenerator(generator)); err == nil || !strings.Contains(err.Error(), "message id") {
		t.Errorf("message id: %v", err)
	}
}

func TestFileSenderRefusesAnUnusableDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := forgemail.NewFileSender(file, logger); err == nil {
		t.Error("a regular file was accepted as the directory")
	}
	if _, err := forgemail.NewFileSender(filepath.Join(file, "sub"), logger); err == nil || !strings.Contains(err.Error(), "create file sender directory") {
		t.Errorf("directory below a file: %v", err)
	}
	if _, err := forgemail.NewFileSender("bad\x00dir", logger); err == nil {
		t.Error("a NUL byte in the directory was accepted")
	}
}

func TestTightenDirectoryReportsWhatItCannotInspect(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits carry no meaning on Windows")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	if err := forgemail.TightenDirectoryForTest(filepath.Join(dir, "missing"), logger); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Errorf("missing directory: %v", err)
	}
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := forgemail.TightenDirectoryForTest(file, logger); err == nil {
		t.Error("a regular file was inspected as a directory")
	}
	// An already private directory is left alone and silent.
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	if err := forgemail.TightenDirectoryForTest(private, slog.New(slog.NewTextHandler(&logs, nil))); err != nil || logs.Len() != 0 {
		t.Errorf("private directory: %v %q", err, logs.String())
	}
}

func TestFileSenderFailsClosedWhenTheDirectoryDisappears(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "outbox")
	sender, err := forgemail.NewFileSender(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sender.Close() }()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), baseMessage())
	if !errors.Is(err, forgemail.ErrFileSender) {
		t.Fatalf("err = %v, want ErrFileSender", err)
	}
}
