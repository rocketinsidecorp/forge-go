package mail_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/mail"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

func TestFileSenderWritesPrivateEMLAndLogsOnlyThePath(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "tmp", "mails")
	var logs bytes.Buffer
	sender, err := forgemail.NewFileSender(dir, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	msg := baseMessage()
	msg.Subject = "Reset your password"
	msg.Text = "link: https://example.com/reset?token=SECRET-TOKEN"
	msg.Bcc = []forgemail.Address{{Email: "audit@example.net"}}
	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".eml") {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	path := filepath.Join(dir, entries[0].Name())
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("file mode = %v, want 0600", info.Mode().Perm())
		}
		dirInfo, _ := os.Stat(dir)
		if dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %v, want 0700", dirInfo.Mode().Perm())
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("file is not a valid message: %v", err)
	}
	if parsed.Header.Get("Subject") != "Reset your password" || !strings.Contains(parsed.Header.Get("X-Forge-Envelope-To"), "audit@example.net") {
		t.Fatalf("headers = %v", parsed.Header)
	}

	logged := logs.String()
	if !strings.Contains(logged, entries[0].Name()) {
		t.Fatalf("log does not mention the path: %s", logged)
	}
	for _, secret := range []string{"SECRET-TOKEN", "alice@example.org", "audit@example.net", "Reset your password", "app@example.com"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaks %q: %s", secret, logged)
		}
	}
}

func TestFileSenderValidatesAndHonoursContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sender, err := forgemail.NewFileSender(dir, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	bad := baseMessage()
	bad.Subject = "x\r\nBcc: evil@example.net"
	if err := sender.Send(context.Background(), bad); !errors.Is(err, forgemail.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sender.Send(ctx, baseMessage()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files written for rejected sends: %v", entries)
	}
}

func TestFileSenderNamesAreUniqueAndConfined(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sender, err := forgemail.NewFileSender(dir, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	for i := 0; i < 20; i++ {
		msg := baseMessage()
		msg.Subject = "../../escape"
		if err := sender.Send(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 20 {
		t.Fatalf("entries = %d, want 20 distinct files", len(entries))
	}
	err = filepath.WalkDir(filepath.Dir(dir), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Dir(path) != dir {
			t.Errorf("file outside the mail directory: %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewFileSenderRejectsEmptyDirectory(t *testing.T) {
	t.Parallel()
	if _, err := forgemail.NewFileSender("", nil); err == nil {
		t.Fatal("empty directory accepted")
	}
}

func TestNewFileSenderTightensAnExistingOpenDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits carry no meaning on Windows")
	}
	mode := func(dir string) fs.FileMode {
		t.Helper()
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	newSender := func(dir string, logs *bytes.Buffer) {
		t.Helper()
		sender, err := forgemail.NewFileSender(dir, slog.New(slog.NewTextHandler(logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sender.Close() })
	}

	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil { // not subject to the umask
		t.Fatal(err)
	}
	var logs bytes.Buffer
	newSender(open, &logs)
	if got := mode(open); got != 0o700 {
		t.Fatalf("mode = %v, want 0700", got)
	}
	if !strings.Contains(logs.String(), "tightened") {
		t.Fatalf("no warning: %q", logs.String())
	}

	// Already private: untouched, including a narrower owner mode.
	private := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(private, 0o500); err != nil {
		t.Fatal(err)
	}
	newSender(private, &bytes.Buffer{})
	if got := mode(private); got != 0o500 {
		t.Fatalf("mode = %v, want 0500", got)
	}
	_ = os.Chmod(private, 0o700)

	// A sticky shared directory is left alone.
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777|fs.ModeSticky); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	newSender(shared, &logs)
	info, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o777 || info.Mode()&fs.ModeSticky == 0 || !strings.Contains(logs.String(), "sticky") {
		t.Fatalf("mode = %v, logs = %q", info.Mode(), logs.String())
	}
}

func TestNewFileSenderDoesNotFollowASymlinkToChangePermissions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits carry no meaning on Windows")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	sender, err := forgemail.NewFileSender(link, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 || !strings.Contains(logs.String(), "symbolic link") {
		t.Fatalf("mode = %v, logs = %q", info.Mode().Perm(), logs.String())
	}
}
