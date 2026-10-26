package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/rocketinsidecorp/forge-go/credentials"
)

// editorRunner opens path in the user's editor and returns when it exits.
type editorRunner func(editor []string, path string) error

// edit decrypts the credentials into a private temporary directory, opens the
// editor, validates the result and re-encrypts it atomically. The temporary
// directory is overwritten and removed on every path out of this function:
// success, editor failure, invalid JSON, a failed write, SIGTERM and SIGHUP.
// Only SIGKILL or a crash can leave it behind.
func (c *credentialsContext) edit(env string) error {
	editor := c.editorCommand()
	if len(editor) == 0 {
		return errors.New("no editor: set $VISUAL or $EDITOR")
	}
	// Catch termination for the whole flow, not only while the editor runs:
	// the default action would end the process without running the deferred
	// cleanup. Signals are acted on at the checkpoints below.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)

	plaintext, keys, _, file, err := c.readPlaintext(env)
	if err != nil {
		return err
	}
	defer clear(plaintext)
	var original bytes.Buffer
	if err := json.Indent(&original, plaintext, "", "  "); err != nil {
		return err
	}
	original.WriteByte('\n')
	defer func() { clear(original.Bytes()) }()

	dir, err := privateTempDir()
	if err != nil {
		return fmt.Errorf("create private temporary directory: %w", err)
	}
	defer func() {
		if cleanupErr := shredDir(dir); cleanupErr != nil {
			fmt.Fprintf(c.stderr, "warning: could not remove %s: %v\n", dir, cleanupErr)
		}
	}()
	path := filepath.Join(dir, env+".json")
	if err := os.WriteFile(path, original.Bytes(), 0o600); err != nil {
		return err
	}
	if err := interrupted(signals, false); err != nil {
		return err
	}

	if err := c.editor(editor, path); err != nil {
		return fmt.Errorf("editor failed; credentials are unchanged: %w", err)
	}
	// An interrupt typed in the editor reached us too; it belongs to the
	// editor, so only termination signals count from here on.
	if err := interrupted(signals, true); err != nil {
		return err
	}
	edited, err := readBounded(path, credentials.MaxPlaintextBytes)
	if err != nil {
		return err
	}
	defer clear(edited)
	if err := credentials.ParseDocument(edited); err != nil {
		return fmt.Errorf("%w; credentials are unchanged (your edits were discarded)", err)
	}
	if !bytes.HasSuffix(edited, []byte("\n")) {
		edited = append(edited, '\n')
	}
	if bytes.Equal(bytes.TrimSpace(edited), bytes.TrimSpace(original.Bytes())) {
		fmt.Fprintln(c.stdout, "no changes")
		return nil
	}
	if err := interrupted(signals, false); err != nil {
		return err
	}
	sealed, err := credentials.Seal(env, edited, keys[0])
	if err != nil {
		return err
	}
	// Refuse to overwrite a file somebody else changed while the editor was
	// open (a merge, a concurrent edit): their update would be lost.
	current, err := credentials.ReadFile(c.dir, env)
	if err != nil || !bytes.Equal(current, file) {
		return errors.New("the credentials file changed while you were editing; credentials are unchanged, edit again")
	}
	if err := credentials.WriteFile(c.dir, env, sealed, true); err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.stdout, "saved %s (key %s)\n", filepath.Join(c.dir, credentials.FileName(env)), keys[0].ID)
	return err
}

// interrupted reports a pending signal as an error, so edit can unwind
// through its deferred cleanup. With skipInterrupt, SIGINT is discarded.
func interrupted(signals <-chan os.Signal, skipInterrupt bool) error {
	for {
		select {
		case sig := <-signals:
			if skipInterrupt && sig == os.Interrupt {
				continue
			}
			return fmt.Errorf("interrupted by %v; credentials are unchanged", sig)
		default:
			return nil
		}
	}
}

// editorCommand returns $VISUAL or $EDITOR split into words. No shell is
// involved, so quoting is not interpreted: wrapper scripts work, shell
// pipelines do not. Editors known to write swap, backup or history files
// receive flags that turn them off, so plaintext does not outlive the
// session.
func (c *credentialsContext) editorCommand() []string {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if value, ok := c.lookup(name); ok {
			if words := strings.Fields(value); len(words) > 0 {
				return hardenEditor(words)
			}
		}
	}
	return nil
}

// hardenEditor adds the flags that keep Vim-family editors from writing swap,
// backup, undo and viminfo files. The editor is recognized by its own name
// and by what the name resolves to, so vi, editor or a wrapper symlinked to
// vim.basic, vim.nox, gvim or nvim are covered. Other editors (a busybox vi,
// nano, Emacs, GUI editors) are launched unchanged and may keep copies; see
// docs/CREDENTIALS.md.
func hardenEditor(words []string) []string {
	if !isVimFamily(words[0]) {
		return words
	}
	return append(words, "-n", "-i", "NONE", "-c", "set noundofile nobackup nowritebackup viminfo=")
}

func isVimFamily(command string) bool {
	names := []string{filepath.Base(command)}
	if resolved, err := exec.LookPath(command); err == nil {
		names = append(names, filepath.Base(resolved))
		if target, err := filepath.EvalSymlinks(resolved); err == nil {
			names = append(names, filepath.Base(target))
		}
	}
	for _, name := range names {
		name = strings.ToLower(strings.TrimSuffix(name, ".exe"))
		for _, prefix := range []string{"vim", "nvim", "gvim", "gview", "mvim", "view", "rvim", "rview"} {
			if name == prefix || strings.HasPrefix(name, prefix+".") || strings.HasPrefix(name, prefix+"-") {
				return true
			}
		}
	}
	return false
}

// editorEnvironment is the environment the editor runs with: the caller's,
// minus every FORGE_ variable. FORGE_MASTER_KEY and the other settings hold
// secrets, and an editor (with its plugins, shell escapes and subprocesses)
// has no use for them.
func editorEnvironment(environ []string) []string {
	kept := make([]string, 0, len(environ))
	for _, entry := range environ {
		if !strings.HasPrefix(strings.ToUpper(entry), "FORGE_") {
			kept = append(kept, entry)
		}
	}
	return kept
}

// runEditorProcess runs the editor attached to the terminal. SIGINT is left to
// the editor (the terminal delivers it to both, as git does); SIGTERM and
// SIGHUP stop the editor so the caller can clean up.
func runEditorProcess(editor []string, path string) error {
	args := append(append([]string(nil), editor[1:]...), path)
	//nolint:noctx // the editor owns the terminal: this function forwards SIGTERM/SIGHUP and leaves SIGINT to it, instead of a context kill
	cmd := exec.Command(editor[0], args...) // #nosec G204 -- the editor is the user's own $VISUAL/$EDITOR
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = editorEnvironment(os.Environ())
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case err := <-done:
			return err
		case sig := <-signals:
			if sig == os.Interrupt {
				continue
			}
			_ = cmd.Process.Kill()
			waitErr := <-done
			_ = waitErr // the exit status of a killed editor is not reported
			return fmt.Errorf("interrupted by %v", sig)
		}
	}
}

// privateTempDir creates a directory only the current user can enter. On
// Linux it prefers /dev/shm, which is memory-backed and never reaches disk.
func privateTempDir() (string, error) {
	if runtime.GOOS == "linux" {
		if dir, err := os.MkdirTemp("/dev/shm", "forge-credentials-"); err == nil {
			return dir, nil
		}
	}
	return os.MkdirTemp("", "forge-credentials-")
}

// shredDir overwrites every regular file under dir with zeros, syncs it, and
// removes the tree. Overwriting is best effort: copy-on-write and journaling
// filesystems may keep old blocks, which is why the directory prefers tmpfs.
func shredDir(dir string) error {
	var firstErr error
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil //nolint:nilerr // an unreadable entry is skipped: the walk still removes the tree, and shredDir reports the first shred failure
		}
		if err := shredFile(path); err != nil && firstErr == nil {
			firstErr = err
		}
		return nil
	})
	removeErr := os.RemoveAll(dir)
	return errors.Join(firstErr, walkErr, removeErr)
}

func shredFile(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0) // #nosec G304 -- a file inside the private directory this command created
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err == nil {
		_, err = io.CopyN(file, zeroReader{}, info.Size())
	}
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path) // #nosec G304 -- the editor file inside the private directory this command created
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		clear(data)
		return nil, fmt.Errorf("edited document exceeds %d bytes", limit)
	}
	return data, nil
}
