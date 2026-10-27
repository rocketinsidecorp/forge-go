package frontend

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// compiledPath is the slash-separated path of a compiled file below the
// application root.
func compiledPath(name string) string { return path.Join(SourceDir, DistDir, name) }

// distEntry is a file or other non-directory already in dist.
type distEntry struct {
	name    string // below app/javascript/dist, slash-separated
	modTime time.Time
}

// tempSuffix ends the names of the temporary files writeAtomic makes.
const tempSuffix = ".forge-tmp"

// tempGrace is how long a temporary file is left alone before Sync removes it
// as the leftover of an interrupted run: a concurrent forge may be about to
// rename it.
const tempGrace = 10 * time.Minute

// existingDist lists everything in dist that is not a directory: compiled
// files, and anything else. dist is generated and the javascript package embeds
// all of it (a README.txt there would be served as an asset), so whatever forge
// did not write is not wanted. A missing directory has none.
func existingDist(root fs.FS) ([]distEntry, error) {
	var found []distEntry
	prefix := path.Join(SourceDir, DistDir)
	err := fs.WalkDir(root, prefix, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		// An entry whose time cannot be read counts as old.
		var modTime time.Time
		if info, err := entry.Info(); err == nil {
			modTime = info.ModTime()
		}
		found = append(found, distEntry{name: strings.TrimPrefix(name, prefix+"/"), modTime: modTime})
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

// requireRealDirs refuses a symlink, or anything that is not a directory, at
// dir or any of its parents below the project root. forge writes compiled files
// there and removes the ones whose source is gone, and os.Root follows a
// symlink that stays inside the project: `dist -> ../../public` would make
// forge delete and overwrite files of public/. A directory that does not exist
// yet is fine, it is created.
func requireRealDirs(project *os.Root, dir string) error {
	current := ""
	for _, part := range strings.Split(dir, "/") {
		current = path.Join(current, part)
		info, err := project.Lstat(filepath.FromSlash(current))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a plain directory (a symlink, or a file): forge compiles into %s and will not write or remove files through it", current, path.Join(SourceDir, DistDir))
		}
	}
	return nil
}

// writeAtomic writes content to target through a temporary file (a random name
// in the same directory) and a rename, so a reader (go build, an interrupted forge) sees the
// old file or the new one, never half of it, and a symlink at target is
// replaced rather than written through.
func writeAtomic(project *os.Root, target string, content []byte) (err error) {
	// A random name, so two forge runs writing the same file do not clash;
	// O_EXCL refuses a name that is somehow taken.
	temporary := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+"."+rand.Text()[:13]+tempSuffix)
	file, err := project.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = project.Remove(temporary)
		}
	}()
	if _, writeErr := file.Write(content); writeErr != nil {
		_ = file.Close()
		return writeErr
	}
	if closeErr := file.Close(); closeErr != nil {
		return closeErr
	}
	return project.Rename(temporary, target)
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
	if err := requireRealDirs(project, path.Join(SourceDir, DistDir)); err != nil {
		return nil, err
	}
	outputs, err := Build(root, Production)
	if err != nil {
		return nil, err
	}
	var changed []string
	for name, content := range outputs {
		if err := requireRealDirs(project, path.Dir(compiledPath(name))); err != nil {
			return nil, err
		}
		target := filepath.FromSlash(compiledPath(name))
		if current, err := project.ReadFile(target); err == nil && bytes.Equal(current, content) {
			continue
		}
		if err := project.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return nil, err
		}
		if err := writeAtomic(project, target, content); err != nil {
			return nil, err
		}
		changed = append(changed, name)
	}
	existing, err := existingDist(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range existing {
		if _, wanted := outputs[entry.name]; wanted {
			continue
		}
		if strings.HasSuffix(entry.name, tempSuffix) && time.Since(entry.modTime) < tempGrace {
			continue // possibly another forge's file, about to be renamed
		}
		if err := project.Remove(filepath.FromSlash(compiledPath(entry.name))); err != nil {
			return nil, err
		}
		changed = append(changed, entry.name)
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
	if err := requireRealDirs(project, path.Join(SourceDir, DistDir)); err != nil {
		return "", err
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
	for _, entry := range existing {
		if _, wanted := outputs[entry.name]; !wanted {
			// An empty path deletes the file for the build.
			replace[filepath.Join(absolute, filepath.FromSlash(compiledPath(entry.name)))] = ""
		}
	}
	encoded, err := json.Marshal(map[string]map[string]string{"Replace": replace})
	if err != nil { // a map of strings cannot fail to marshal; there is no seam to test the branch
		return "", fmt.Errorf("encode overlay: %w", err)
	}
	overlay := filepath.Join(outDir, overlayFile)
	if err := os.WriteFile(overlay, encoded, 0o600); err != nil {
		return "", err
	}
	return overlay, nil
}
