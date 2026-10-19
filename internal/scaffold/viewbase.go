package scaffold

import (
	"bytes"
	"fmt"
	"go/format"
	"path/filepath"
	"strings"
)

// BaseFile is one file of an application's view base.
type BaseFile struct {
	Path    string // application-relative, OS-specific separators
	Content []byte
}

// ViewBase returns the files `forge new` generates for server-rendered HTML:
// the embedding views package, its test, the application layout and the flash
// partial. `forge generate view` and `forge generate scaffold` add them to an
// application that lacks them, so both agree with `forge new` on one layout.
// name is the application name, used in the default page title.
func ViewBase(name string) ([]BaseFile, error) {
	opts, err := Normalize(Options{Name: name})
	if err != nil {
		return nil, err
	}
	return renderSelected(opts, isViewBase)
}

// AuthenticationFiles returns the sign-in, sign-out, two-factor and
// password-reset pages `forge new` generates for a PostgreSQL application:
// app/adapters/htmlauth (handlers and tests) and app/views/sessions and
// app/views/passwords. `forge generate authentication` adds them to an
// application that lacks them, so both produce the same files. name is the
// application name and module its Go module path.
func AuthenticationFiles(name, module string) ([]BaseFile, error) {
	opts, err := Normalize(Options{Name: name, Module: module, Database: "postgresql"})
	if err != nil {
		return nil, err
	}
	return renderSelected(opts, func(f file) bool { return f.scope == withIdentity })
}

// renderSelected renders the files keep selects, formatting Go sources.
func renderSelected(opts Options, keep func(file) bool) ([]BaseFile, error) {
	data := templateData{Options: opts}
	plain, err := parseTemplates(false)
	if err != nil {
		return nil, err
	}
	views, err := parseTemplates(true)
	if err != nil {
		return nil, err
	}
	var result []BaseFile
	for _, f := range files {
		if !keep(f) {
			continue
		}
		set := plain
		if strings.HasSuffix(f.template, viewTemplateSuffix) {
			set = views
		}
		var buffer bytes.Buffer
		if err := set.ExecuteTemplate(&buffer, f.template, data); err != nil {
			return nil, fmt.Errorf("render %s: %w", f.template, err)
		}
		content := buffer.Bytes()
		path := f.path(opts.Name)
		if strings.HasSuffix(path, ".go") {
			if content, err = format.Source(content); err != nil {
				return nil, fmt.Errorf("format %s: %w", path, err)
			}
		}
		result = append(result, BaseFile{Path: path, Content: content})
	}
	return result, nil
}

// isViewBase selects the files under app/views that every application has;
// the authentication pages belong to AuthenticationFiles.
func isViewBase(f file) bool {
	return f.scope == everyApplication && strings.HasPrefix(f.path(""), filepath.Join("app", "views")+string(filepath.Separator))
}
