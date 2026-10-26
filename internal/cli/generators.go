package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// The generators that do not take a resource command line (controller,
// mailer, queue job) share what this file holds: opening the application,
// validating names, and refusing names that would not be safe Go identifiers,
// file names or URL segments.

// snakeName is a lowercase snake_case word list: letters and digits, single
// underscores between words, starting with a letter.
var snakeName = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z][a-z0-9]*)*$`)

const maxGeneratorNameLength = 40

// openApplication opens the application in root and returns its Go module
// path. The caller closes the root.
func openApplication(root string) (*os.Root, string, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", errors.New("go.mod not found: run this inside a Forge application")
	}
	goMod, err := projectRoot.ReadFile("go.mod")
	if err != nil {
		_ = projectRoot.Close()
		return nil, "", errors.New("go.mod not found: run this inside a Forge application")
	}
	module, _, _ := inspectGoMod(goMod)
	if module == "" {
		_ = projectRoot.Close()
		return nil, "", errors.New("go.mod has no module declaration")
	}
	return projectRoot, module, nil
}

// Refusals of the generators that write pages in an API-only application
// (`forge new --api`), worded after Forge-Rust's: it has no views package,
// layout, assets or browser middleware, and a generator never adds them to
// such an application.
const (
	apiOnlyViewRefusal           = "this application has no views (it was created with `forge new --api`): pages need the layout, the assets and the browser middleware (CSRF, method override, flash) of a full-stack application. Create one with `forge new NAME` and move your code into it"
	apiOnlyScaffoldRefusal       = "this application has no views (it was created with `forge new --api`). The scaffold generates server-rendered HTML pages next to its JSON API; `forge generate resource` writes the JSON API alone"
	apiOnlyControllerRefusal     = "this application has no views (it was created with `forge new --api`): an HTML controller renders pages. Pass --api for JSON endpoints under /v1/NAME"
	apiOnlyAuthenticationRefusal = "this application has no views (it was created with `forge new --api`). The generator adds server-rendered sign-in pages, which need the views and the browser middleware (CSRF) of a full-stack application, and the JSON authentication APIs of --api-auth sit beside them. Create one with `forge new NAME` and move your code into it"
)

// apiOnlyApplication reports whether the application was created with `forge
// new --api`: its app/bootstrap/bootstrap.go carries scaffold.APIOnlyMarker.
// An application without that file, or whose bootstrap.go lost the marker, is
// treated as full-stack, so the generators may add the views base to it.
func apiOnlyApplication(projectRoot *os.Root) bool {
	source, err := projectRoot.ReadFile(filepath.Join("app", "bootstrap", "bootstrap.go"))
	return err == nil && strings.Contains(string(source), scaffold.APIOnlyMarker)
}

// apiOnlyAt is apiOnlyApplication for the application in root. A root that
// cannot be opened is not API-only: the generator reports it itself.
func apiOnlyAt(root string) bool {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return false
	}
	defer func() { _ = projectRoot.Close() }()
	return apiOnlyApplication(projectRoot)
}

// validateGeneratorWord checks a name that becomes a Go identifier, a file
// name and a path segment: kind says what it is in the message.
func validateGeneratorWord(kind, name string) error {
	switch {
	case len(name) > maxGeneratorNameLength || !snakeName.MatchString(name):
		return fmt.Errorf("%s %q must be snake_case (lowercase letters and digits, words separated by single underscores, starting with a letter, at most %d characters)", kind, name, maxGeneratorNameLength)
	case slices.Contains(goReservedNames, name):
		return fmt.Errorf("%s %q is a Go keyword, predeclared identifier or a package the generated code imports; choose another name", kind, name)
	case hasFileNameSuffix(name):
		return fmt.Errorf("%s %q ends in a word Go treats as a file-name build constraint (such as _test or _linux); choose another name", kind, name)
	}
	return nil
}

// splitFlags separates the flags a generator takes from its positional
// arguments. A flag not in known is an error; none of them takes a value.
func splitFlags(args []string, usage string, known ...string) (flags map[string]bool, positional []string, err error) {
	flags = make(map[string]bool, len(known))
	for _, arg := range args {
		switch {
		case slices.Contains(known, arg):
			if flags[arg] {
				return nil, nil, fmt.Errorf("%s given more than once", arg)
			}
			flags[arg] = true
		case strings.HasPrefix(arg, "-"):
			return nil, nil, usageError{fmt.Sprintf("unknown flag %q\n%s", arg, usage)}
		default:
			positional = append(positional, arg)
		}
	}
	return flags, positional, nil
}
