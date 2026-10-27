package scaffold_test

import (
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// TestEveryApplicationGetsTheArchitectureTest requires app/architecture_test.go
// in every kind of application, written against this framework's module path
// and already formatted. internal/cli proves what the file enforces.
func TestEveryApplicationGetsTheArchitectureTest(t *testing.T) {
	for _, options := range []scaffold.Options{
		{Name: "shop", Database: "postgresql"},
		{Name: "shop", Database: "sqlite"},
		{Name: "shop", Database: "mysql", Module: "example.com/shop"},
		{Name: "shop"},
		{Name: "shop", Database: "sqlite", API: true},
	} {
		options.Dir = filepath.Join(t.TempDir(), "shop")
		if _, err := scaffold.Generate(options); err != nil {
			t.Fatalf("generate %+v: %v", options, err)
		}
		content, err := os.ReadFile(filepath.Join(options.Dir, "app", "architecture_test.go"))
		if err != nil {
			t.Fatalf("%+v: %v", options, err)
		}
		if !strings.Contains(string(content), `const frameworkModule = "`+scaffold.FrameworkModule+`"`) {
			t.Errorf("%+v: the architecture test does not name the framework module %s", options, scaffold.FrameworkModule)
		}
		if formatted, err := format.Source(content); err != nil || string(formatted) != string(content) {
			t.Errorf("%+v: the architecture test is not gofmt-clean (%v)", options, err)
		}
		if !strings.Contains(string(content), "package app_test") {
			t.Errorf("%+v: unexpected package clause", options)
		}
	}
}
