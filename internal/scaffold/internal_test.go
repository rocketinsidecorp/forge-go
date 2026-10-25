package scaffold

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"text/template"
)

func TestCleanupRemovesOnlyWhatGenerateCreated(t *testing.T) {
	t.Parallel()
	cause := errors.New("disk full")

	created := filepath.Join(t.TempDir(), "new-app")
	if err := os.MkdirAll(filepath.Join(created, "cmd"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(created, true, cause); !errors.Is(err, cause) || err != cause {
		t.Errorf("cleanup returned %v, want the cause unchanged", err)
	}
	if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the partially generated directory is still there: %v", err)
	}

	// A directory that existed before (empty, given by the user) stays.
	existing := t.TempDir()
	if err := cleanup(existing, false, cause); err != cause {
		t.Errorf("cleanup returned %v", err)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Errorf("a directory Generate did not create was removed: %v", err)
	}

	// When even the removal fails both errors are reported.
	err := cleanup("bad\x00path", true, cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "remove partially generated") {
		t.Errorf("cleanup with a failing removal returned %v", err)
	}
}

// A later file's {{define}} replaces an earlier file's template in a set
// parsed with ParseFS; file templates refuse {{define}} and {{block}}.
func TestFileTemplatesRefuseDefineAndBlock(t *testing.T) {
	t.Parallel()
	attack := fstest.MapFS{
		"templates/ci.yml.tmpl":  {Data: []byte("steps: gated\n")},
		"templates/zzz.txt.tmpl": {Data: []byte(`{{define "ci.yml.tmpl"}}steps: evil{{end}}text`)},
	}
	// The threat: ParseFS alone lets the later file win.
	replaced, err := template.New("").ParseFS(attack, "templates/*.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := replaced.ExecuteTemplate(&out, "ci.yml.tmpl", nil); err != nil || out.String() != "steps: evil" {
		t.Fatalf("ParseFS no longer lets a later {{define}} win (%q, %v): revisit this test", out.String(), err)
	}
	for name, text := range map[string]string{
		"define": `{{define "ci.yml.tmpl"}}steps: evil{{end}}text`,
		"other":  `{{define "helper"}}x{{end}}text`,
		"block":  `{{block "ci.yml.tmpl" .}}steps: evil{{end}}`,
	} {
		files := fstest.MapFS{
			"templates/ci.yml.tmpl":  {Data: []byte("steps: gated\n")},
			"templates/zzz.txt.tmpl": {Data: []byte(text)},
		}
		if _, err := parseFileTemplates(files, fileTemplates); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	clean := fstest.MapFS{"templates/a.tmpl": {Data: []byte("{{if .X}}a{{end}}\n")}, "templates/b.tmpl": {Data: []byte("")}}
	if _, err := parseFileTemplates(clean, fileTemplates); err != nil {
		t.Errorf("rejected plain file templates: %v", err)
	}
	broken := fstest.MapFS{"templates/a.tmpl": {Data: []byte("{{if}}")}}
	if _, err := parseFileTemplates(broken, fileTemplates); err == nil {
		t.Error("accepted a template that does not parse")
	}
	if _, err := parseFileTemplates(clean, "["); err == nil {
		t.Error("accepted a malformed pattern")
	}
}

// The set Generate renders holds exactly one template per file, under the
// file's base name, each with the tree the file parses to on its own: no
// file template can stand in for another (ci.yml.tmpl is what the gate reads).
func TestFileTemplateSetHoldsEachFileAsParsedAlone(t *testing.T) {
	t.Parallel()
	set, err := parseTemplates(false)
	if err != nil {
		t.Fatal(err)
	}
	files, err := fs.Glob(templateFS, fileTemplates)
	if err != nil {
		t.Fatal(err)
	}
	var want, got []string
	for _, file := range files {
		want = append(want, path.Base(file))
	}
	for _, defined := range set.Templates() {
		got = append(got, defined.Name())
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Contains(want, "ci.yml.tmpl") || !slices.Equal(got, want) {
		t.Fatalf("the set holds %v, the files are %v", got, want)
	}
	for _, file := range files {
		content, err := fs.ReadFile(templateFS, file)
		if err != nil {
			t.Fatal(err)
		}
		alone, err := template.New(path.Base(file)).Parse(string(content))
		if err != nil {
			t.Fatal(err)
		}
		inSet := set.Lookup(path.Base(file))
		if inSet == nil || inSet.Tree == nil || alone.Tree == nil || inSet.Root.String() != alone.Root.String() {
			t.Errorf("%s: the set does not hold the template the file parses to", file)
		}
	}
}
