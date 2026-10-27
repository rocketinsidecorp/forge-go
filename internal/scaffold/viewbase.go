package scaffold

import (
	"bytes"
	"fmt"
	"go/format"
	"path/filepath"
	"slices"
	"strings"
)

// BaseFile is one file of an application's view base.
type BaseFile struct {
	Path    string // application-relative, OS-specific separators
	Content []byte
}

// ViewBase returns the files `forge new` generates for server-rendered HTML:
// the embedding views package, its test, the application layout and the flash
// partial, plus what the layout loads: the asset bundle (app/assets, the
// stylesheet and logo), the JavaScript (app/javascript) and their Go packages.
// `forge generate view` and `forge generate scaffold` add them to an
// application that lacks them, so both agree with `forge new` on one layout.
// name is the application name, used in the default page title; module is
// its Go module path, which app/assets imports app/javascript through.
func ViewBase(name, module string) ([]BaseFile, error) {
	opts, err := Normalize(Options{Name: name, Module: module})
	if err != nil {
		return nil, err
	}
	rendered, err := renderSelected(opts, isViewBase)
	if err != nil {
		return nil, err
	}
	frontend, err := frontendAssets(opts)
	if err != nil {
		return nil, err
	}
	for _, asset := range append(slices.Clone(assets), frontend...) {
		if asset.target == ".gitattributes" {
			// A project's own .gitattributes is not ours to overwrite or
			// merge: only forge new writes it.
			continue
		}
		content, err := templateFS.ReadFile(asset.source)
		if err != nil {
			return nil, fmt.Errorf("read asset %s: %w", asset.source, err)
		}
		rendered = append(rendered, BaseFile{Path: asset.target, Content: content})
	}
	compiled, err := compiledFiles(opts)
	if err != nil {
		return nil, err
	}
	return append(rendered, compiled...), nil
}

// AuthenticationFiles returns the Forge identity files `forge new --auth`
// generates: app/bootstrap/identity.go and its test (the wiring the routes
// seam calls), app/adapters/htmlauth (handlers and tests), app/views/sessions,
// app/views/passwords, app/views/mfa and their English text,
// config/locales/authentication.en.json. `forge generate authentication`
// adds them to an application that lacks them, so both produce the same
// files. opts names the application, its module, its database (any of
// Databases) and APIAuth; Auth is implied.
func AuthenticationFiles(opts Options) ([]BaseFile, error) {
	opts.Auth = true
	opts, err := Normalize(opts)
	if err != nil {
		return nil, err
	}
	files, err := renderSelected(opts, func(f file) bool { return f.scope == withIdentity })
	if err != nil {
		return nil, err
	}
	english, err := AuthenticationTranslation("en")
	if err != nil {
		return nil, err
	}
	return append(files, english), nil
}

// renderSelected renders the files keep selects, formatting Go sources.
func renderSelected(opts Options, keep func(file) bool) ([]BaseFile, error) {
	data := newTemplateData(opts)
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

// isViewBase selects the files under app/views, app/assets and app/javascript
// that every full-stack application has; the authentication pages belong to
// AuthenticationFiles.
func isViewBase(f file) bool {
	if f.scope != withViews && f.scope != withTypeScript {
		return false
	}
	path := f.path("")
	for _, dir := range []string{"views", "assets", "javascript"} {
		if strings.HasPrefix(path, filepath.Join("app", dir)+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// AgentsFile is AGENTS.md, the guide for AI coding agents `forge new`
// writes. `forge generate authentication` and `forge generate tenancy`
// render it again when they change what it describes.
const AgentsFile = "AGENTS.md"

// RenderAgents renders AGENTS.md for an application like opts.
func RenderAgents(opts Options) ([]byte, error) {
	opts, err := Normalize(opts)
	if err != nil {
		return nil, err
	}
	files, err := renderSelected(opts, func(f file) bool { return f.template == "AGENTS.md.tmpl" })
	if err != nil {
		return nil, err
	}
	return files[0].Content, nil
}
