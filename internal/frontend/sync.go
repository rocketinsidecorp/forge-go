package frontend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// compiledPath is the slash-separated path of a compiled file below the
// application root.
func compiledPath(name string) string { return path.Join(SourceDir, DistDir, name) }

// existingDist lists the compiled .js files already on disk, as paths below
// app/javascript/dist. A missing directory has none.
func existingDist(root fs.FS) ([]string, error) {
	var found []string
	err := fs.WalkDir(root, path.Join(SourceDir, DistDir), func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() && strings.HasSuffix(name, ".js") {
			found = append(found, strings.TrimPrefix(name, path.Join(SourceDir, DistDir)+"/"))
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return found, nil
}

// Sync compiles the application at dir for production into
// app/javascript/dist, the files its javascript package embeds. It writes a
// file only when its content changes (so timestamps and version control stay
// quiet when nothing changed) and removes compiled files whose source is gone.
// It returns the paths it wrote or removed, below dist, sorted. An application
// without a TypeScript front end is left alone.
func Sync(dir string) ([]string, error) {
	project, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = project.Close() }()
	root := project.FS()
	if !IsTypeScript(root) {
		return nil, nil
	}
	outputs, err := Build(root, Production)
	if err != nil {
		return nil, err
	}
	var changed []string
	for name, content := range outputs {
		target := filepath.FromSlash(compiledPath(name))
		if current, err := project.ReadFile(target); err == nil && bytes.Equal(current, content) {
			continue
		}
		if err := project.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return nil, err
		}
		if err := project.WriteFile(target, content, 0o600); err != nil {
			return nil, err
		}
		changed = append(changed, name)
	}
	existing, err := existingDist(root)
	if err != nil {
		return nil, err
	}
	for _, name := range existing {
		if _, wanted := outputs[name]; wanted {
			continue
		}
		if err := project.Remove(filepath.FromSlash(compiledPath(name))); err != nil {
			return nil, err
		}
		changed = append(changed, name)
	}
	slices.Sort(changed)
	return changed, nil
}

// overlayFile is the name of the JSON file Overlay writes into its output
// directory, in the format of `go build -overlay`.
const overlayFile = "overlay.json"

// Overlay compiles the application at dir for development, with inline source
// maps, into outDir and writes outDir/overlay.json: a `go build -overlay` file
// that makes the compiled files appear at app/javascript/dist for that build
// (and hides the ones whose source is gone). The working tree is not touched,
// so the committed production output stays as it is. It returns the overlay's
// path, or "" when the application has no TypeScript front end.
func Overlay(dir, outDir string) (string, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	project, err := os.OpenRoot(absolute)
	if err != nil {
		return "", err
	}
	defer func() { _ = project.Close() }()
	root := project.FS()
	if !IsTypeScript(root) {
		return "", nil
	}
	outputs, err := Build(root, Development)
	if err != nil {
		return "", err
	}
	replace := make(map[string]string, len(outputs))
	for name, content := range outputs {
		target := filepath.Join(outDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return "", err
		}
		replace[filepath.Join(absolute, filepath.FromSlash(compiledPath(name)))] = target
	}
	existing, err := existingDist(root)
	if err != nil {
		return "", err
	}
	for _, name := range existing {
		if _, wanted := outputs[name]; !wanted {
			// An empty path deletes the file for the build.
			replace[filepath.Join(absolute, filepath.FromSlash(compiledPath(name)))] = ""
		}
	}
	encoded, err := json.Marshal(map[string]map[string]string{"Replace": replace})
	if err != nil {
		return "", fmt.Errorf("encode overlay: %w", err)
	}
	overlay := filepath.Join(outDir, overlayFile)
	if err := os.WriteFile(overlay, encoded, 0o600); err != nil {
		return "", err
	}
	return overlay, nil
}
