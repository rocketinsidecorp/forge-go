package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// ErrFileSender is returned when the development sender cannot store a
// message.
var ErrFileSender = fault.New("mail_file_failed", "could not write the development email file", fault.CategoryInternal, 0)

// FileSender is the development sender: it writes each message as an .eml
// file (RFC 5322, openable in any mail client) and logs only the file path,
// never headers, recipients or bodies. Files are created 0600 in a 0700
// directory and cannot escape it, even through symlinks. A directory that
// already exists with group or other permissions is tightened to 0700 (the
// owner bits are kept) so file names and timing stay private too; one that
// cannot be tightened is an error. A sticky directory such as /tmp is a
// shared system location and is left alone with a warning (the files in it
// are 0600 anyway).
//
// It exists to read mail without a mail server. Never use it in production:
// config validation and FromConfig refuse it there.
type FileSender struct {
	root   *os.Root
	dir    string
	logger *slog.Logger
	now    func() time.Time
}

// NewFileSender creates dir (0700) if needed and returns a sender writing
// into it. A nil logger uses slog.Default().
func NewFileSender(dir string, logger *slog.Logger) (*FileSender, error) {
	if dir == "" || strings.ContainsRune(dir, 0) {
		return nil, errors.New("mail: file sender directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mail: create file sender directory: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := tightenDirectory(dir, logger); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("mail: open file sender directory: %w", err)
	}
	// Mail written to files is never delivered. Say so loudly, once, so a
	// misconfigured deployment (a development URL that survived into
	// production) shows up in the first lines of the log instead of as
	// customers who never receive their email.
	logger.Warn("development mail sender active: messages are written to files and NOT delivered", "dir", dir)
	return &FileSender{root: root, dir: dir, logger: logger, now: time.Now}, nil
}

// tightenDirectory removes group and other access from an existing directory.
func tightenDirectory(dir string, logger *slog.Logger) error {
	if runtime.GOOS == "windows" {
		return nil // permission bits carry no meaning there
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("mail: inspect file sender directory: %w", err)
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if info.Mode()&os.ModeSticky != 0 {
		logger.Warn("development mail directory is shared (sticky) and was not tightened", "dir", dir)
		return nil
	}
	if err := os.Chmod(dir, info.Mode()&^0o077); err != nil {
		return fmt.Errorf("mail: tighten file sender directory: %w", err)
	}
	logger.Warn("development mail directory was group- or world-accessible; tightened to 0700", "dir", dir)
	return nil
}

// Close releases the directory handle.
func (s *FileSender) Close() error { return s.root.Close() }

// Send validates and encodes msg and stores it as <time>-<uuid>.eml. A
// leading X-Forge-Envelope-To header lists every recipient, including Bcc
// ones, which no real header carries.
func (s *FileSender) Send(ctx context.Context, msg Message) error {
	if err := ctx.Err(); err != nil {
		return transient("file", 0, err)
	}
	encoded, err := Encode(msg)
	if err != nil {
		return err
	}
	id, err := uuid.New()
	if err != nil {
		return ErrFileSender.WithCause(err)
	}
	name := s.now().UTC().Format("20060102T150405Z") + "-" + id.String() + ".eml"
	file, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ErrFileSender.WithCause(err)
	}
	data := append([]byte(foldHeader("X-Forge-Envelope-To", strings.Join(encoded.Recipients, ", "))+"\r\n"), encoded.Data...)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = s.root.Remove(name)
		return ErrFileSender.WithCause(err)
	}
	if err := file.Close(); err != nil {
		return ErrFileSender.WithCause(err)
	}
	s.logger.InfoContext(ctx, "mail written to file", "path", s.dir+string(os.PathSeparator)+name)
	return nil
}
