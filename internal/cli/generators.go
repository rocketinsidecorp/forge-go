package cli

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
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
