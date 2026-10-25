package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scan for translation keys is bounded: a tree with more source files
// than any application has stops with a clear error, and a file too large to
// be source is skipped instead of read into memory.
func TestI18nCheckBoundsItsScanOfTheSource(t *testing.T) {
	t.Parallel()
	root := i18nProject(t, map[string]string{"config/locales/en.json": `{"en": {"greeting": "Hello"}}`})
	big := filepath.Join(root, "app", "views", "huge.gohtml")
	if err := os.MkdirAll(filepath.Dir(big), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, []byte(`{{ t "from_a_file_too_large_to_be_source" }}`+strings.Repeat(" ", maxScannedFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := checkTranslations(root)
	if err != nil {
		t.Fatalf("a large file stopped the check: %v", err)
	}
	for _, missing := range report.Missing {
		if strings.Contains(fmt.Sprint(missing), "from_a_file_too_large_to_be_source") {
			t.Errorf("a file over the size limit was read: %+v", report.Missing)
		}
	}

	generated := filepath.Join(root, "app", "generated")
	if err := os.MkdirAll(generated, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := range maxScannedFiles + 1 {
		if err := os.WriteFile(filepath.Join(generated, fmt.Sprintf("f%05d.go", i)), []byte("package generated\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := checkTranslations(root); err == nil || !strings.Contains(err.Error(), "source files to scan") {
		t.Errorf("a tree with more than %d source files: %v", maxScannedFiles, err)
	}
}
