package scaffold

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/rocketinsidecorp/forge-go/credentials"
)

// deepDir returns a path of exactly length bytes below base, made of
// directory names under the 255 bytes a name may have. Nothing is created.
func deepDir(base string, length int) string {
	path := base
	for len(path) < length {
		room := length - len(path) - 1 // the separator
		if room > 200 {
			room = 200
		}
		if room < 1 {
			// Not enough room for another name: lengthen the last one.
			return path + strings.Repeat("x", length-len(path))
		}
		path = filepath.Join(path, strings.Repeat("d", room))
	}
	return path
}

// A generation that fails while writing removes the directory it created and
// says why. The writes are made to fail by their path: a path of a system's
// PATH_MAX bytes or more cannot be opened, so the target directory is given
// the length at which the files of one phase still fit and the first file of
// the next one does not.
func TestGenerateRemovesWhatItCreatedWhenAWriteFails(t *testing.T) {
	// Not parallel: very long paths make the temporary directory slow to
	// clean up and the phases share one calculation.
	const pathMax = 4096
	opts, err := Normalize(Options{Name: "shop", Database: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	longest := func(paths ...string) int {
		n := 0
		for _, p := range paths {
			n = max(n, len(p))
		}
		return n
	}
	var generated []string
	for _, f := range selectedFiles(opts) {
		generated = append(generated, f.path(opts.Name))
	}
	var copied []string
	copiedAssets, err := copiedFiles(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range copiedAssets {
		copied = append(copied, a.target)
	}
	compiled, err := compiledFiles(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range compiled {
		copied = append(copied, f.Path)
	}
	var observability []string
	if err := fs.WalkDir(templateFS, observabilityDir, func(source string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && !entry.IsDir() {
			observability = append(observability, strings.TrimPrefix(source, "templates/"))
		}
		return walkErr
	}); err != nil {
		t.Fatal(err)
	}
	creds := []string{
		filepath.Join(credentials.DefaultDir, credentials.KeyFileName("development")),
		filepath.Join(credentials.DefaultDir, credentials.FileName("development")),
	}

	phases := []struct {
		name  string
		paths []string
		want  string
	}{
		{"rendered files", generated, ""},
		{"copied assets", copied, ""},
		{"observability configuration", observability, "observability"},
		{"development credentials", creds, "credentials"},
	}
	before := 0
	tested := 0
	for _, phase := range phases {
		size := longest(phase.paths...)
		if size <= before {
			before = max(before, size)
			continue // never the first to hit the limit: nothing to learn here
		}
		length := pathMax - 1 - 1 - before // the longest earlier path still fits
		base := t.TempDir()
		dir := deepDir(base, length)
		o := opts
		o.Dir = dir
		_, err := Generate(o)
		switch {
		case err == nil:
			t.Errorf("%s: the generation succeeded with a %d byte directory", phase.name, len(dir))
		case !errors.Is(err, syscall.ENAMETOOLONG):
			t.Errorf("%s: err = %v, want the path to be too long", phase.name, err)
		case phase.want != "" && !strings.Contains(err.Error(), phase.want):
			t.Errorf("%s: err = %v, want it to mention %q", phase.name, err, phase.want)
		}
		if _, statErr := os.Stat(dir); statErr == nil {
			t.Errorf("%s: the half-generated directory was left behind", phase.name)
		}
		tested++
		before = size
	}
	if tested < 2 {
		t.Fatalf("only %d phase(s) could be made to fail", tested)
	}
}
