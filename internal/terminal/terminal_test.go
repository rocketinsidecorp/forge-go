package terminal

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestReadLine(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"secret\n":           "secret",
		"secret\r\n":         "secret",
		"secret":             "secret",
		"  spaced  \nnext\n": "  spaced  ",
		"\n":                 "",
		strings.Repeat("x", MaxSecretBytes) + "\n": strings.Repeat("x", MaxSecretBytes),
	} {
		got, err := ReadLine(strings.NewReader(input))
		if err != nil || string(got) != want {
			t.Errorf("ReadLine(%.20q) = %.20q, %v; want %.20q", input, got, err, want)
		}
	}
	if _, err := ReadLine(strings.NewReader("")); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("empty input: %v", err)
	}
	for _, input := range []string{strings.Repeat("x", MaxSecretBytes+1), strings.Repeat("x", MaxSecretBytes+1) + "\n", strings.Repeat("x", 3*MaxSecretBytes)} {
		if _, err := ReadLine(strings.NewReader(input)); !errors.Is(err, ErrTooLong) {
			t.Errorf("%d bytes: %v, want ErrTooLong", len(input), err)
		}
	}
}

func TestIsTerminalRejectsFilesAndPipes(t *testing.T) {
	t.Parallel()
	file, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	if IsTerminal(file) || IsTerminal(reader) || IsTerminal(nil) {
		t.Fatal("regular files and pipes are not terminals")
	}
	if _, err := ReadSecret(reader); err == nil {
		t.Fatal("ReadSecret must refuse a pipe: it cannot hide input there")
	}
}
