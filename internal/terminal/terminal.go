// Package terminal reads a secret typed at an interactive terminal without
// echoing it. It supports Linux and macOS; elsewhere IsTerminal reports false
// so callers fall back to piped input.
package terminal

import (
	"bufio"
	"errors"
	"io"
	"os"
)

// MaxSecretBytes bounds a secret line; longer input is rejected.
const MaxSecretBytes = 4096

// ErrTooLong is returned for a line longer than MaxSecretBytes.
var ErrTooLong = errors.New("terminal: input line is too long")

// IsTerminal reports whether f is an interactive terminal whose echo this
// package can switch off.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	_, err := getState(int(f.Fd())) // #nosec G115 -- file descriptors are small non-negative integers
	return err == nil
}

// ReadSecret reads one line from the terminal f with echo switched off and
// restores the terminal afterwards. The line ending is not part of the
// result.
func ReadSecret(f *os.File) ([]byte, error) {
	fd := int(f.Fd()) // #nosec G115 -- file descriptors are small non-negative integers
	state, err := getState(fd)
	if err != nil {
		return nil, err
	}
	if err := disableEcho(fd, state); err != nil {
		return nil, err
	}
	line, readErr := ReadLine(f)
	if err := restore(fd, state); err != nil {
		return nil, errors.Join(readErr, err)
	}
	return line, readErr
}

// ReadLine reads one line of at most MaxSecretBytes from r, without its
// "\n" or "\r\n" ending. Input that ends without a newline is a line too;
// empty input is io.ErrUnexpectedEOF.
func ReadLine(r io.Reader) ([]byte, error) {
	reader := bufio.NewReaderSize(io.LimitReader(r, MaxSecretBytes+2), MaxSecretBytes+2)
	line, err := reader.ReadSlice('\n')
	switch {
	case errors.Is(err, io.EOF) && len(line) == 0:
		return nil, io.ErrUnexpectedEOF
	case errors.Is(err, bufio.ErrBufferFull), errors.Is(err, io.EOF) && len(line) > MaxSecretBytes:
		return nil, ErrTooLong
	case err != nil && !errors.Is(err, io.EOF):
		return nil, err
	}
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
	}
	if len(line) > MaxSecretBytes {
		return nil, ErrTooLong
	}
	return append([]byte(nil), line...), nil
}
