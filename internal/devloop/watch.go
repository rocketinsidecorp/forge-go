package devloop

import (
	"context"
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

const (
	// DefaultPollInterval is how often the tree is scanned.
	DefaultPollInterval = 500 * time.Millisecond
	// DefaultDebounce is how long the tree must stay quiet after a change
	// before one batch is reported, so a save that touches several files, or
	// an editor's write-rename dance, causes one rebuild.
	DefaultDebounce = 300 * time.Millisecond
	// DefaultMaxFiles bounds the scan: a watched tree larger than this is
	// almost certainly missing an ignore rule, and polling it would burn CPU.
	DefaultMaxFiles = 50_000

	// maxReportedPaths bounds Change.Paths; Change.Count has the true total.
	maxReportedPaths = 10
)

// ErrTooManyFiles is returned when the watched tree exceeds Options.MaxFiles.
var ErrTooManyFiles = errors.New("devloop: too many watched files")

// Rules decide which parts of the tree matter. The defaults watch Go sources
// (not tests), templates, assets, migrations and the development env files,
// and skip dot-directories, build output, vendored code and runtime data.
type Rules struct {
	// IgnoreDirs are directory base names that are never entered.
	IgnoreDirs []string
	// Extensions are the file extensions, with the dot, that trigger a
	// rebuild.
	Extensions []string
	// Names are exact base names (go.mod, go.sum) that trigger a rebuild.
	Names []string
	// EnvFiles are file names in the root that trigger a restart without a
	// rebuild: the environment is read again when the application starts.
	EnvFiles []string
	// IgnoreFiles are slash-separated root-relative paths that never
	// trigger anything, such as the generated db/schema.sql.
	IgnoreFiles []string
}

// DefaultRules returns the rules `forge dev` uses.
func DefaultRules() Rules {
	return Rules{
		IgnoreDirs: []string{"tmp", "vendor", "node_modules", "bin", "storage", "log", "logs", "coverage", "dist"},
		Extensions: []string{
			".go", ".gohtml", ".html", ".tmpl", ".css", ".js", ".mjs", ".json", ".sql", ".txt",
			".svg", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".woff", ".woff2", ".ttf",
		},
		Names:       []string{"go.mod", "go.sum"},
		EnvFiles:    []string{".env.development", ".env.local"},
		IgnoreFiles: []string{"db/schema.sql"},
	}
}

func (r Rules) skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || slices.Contains(r.IgnoreDirs, name)
}

// classify reports whether rel (slash-separated, relative to the root) is
// watched and whether it only needs a restart.
func (r Rules) classify(rel string) (watched, envOnly bool) {
	if slices.Contains(r.IgnoreFiles, rel) {
		return false, false
	}
	base := path.Base(rel)
	if !strings.Contains(rel, "/") && slices.Contains(r.EnvFiles, base) {
		return true, true
	}
	if strings.HasSuffix(base, "~") || strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, ".#") {
		return false, false
	}
	if slices.Contains(r.Names, base) || slices.Contains(r.Extensions, path.Ext(base)) {
		return true, false
	}
	return false, false
}

// Options configures Watch. Zero values take the defaults.
type Options struct {
	Root         string
	PollInterval time.Duration
	Debounce     time.Duration
	MaxFiles     int
	Rules        *Rules
	// Logf reports scan failures after startup, once per run of failures;
	// nil discards them.
	Logf func(format string, args ...any)
}

// Change is one debounced batch of file changes.
type Change struct {
	// Paths are up to ten changed files, slash-separated and relative to the
	// root, sorted.
	Paths []string
	// Count is the number of changed files in the batch.
	Count int
	// EnvOnly is true when every changed file is an env file, so a restart
	// without a rebuild is enough.
	EnvOnly bool
}

type fileState struct {
	modTime time.Time
	size    int64
	envOnly bool
}

// Watch polls the tree under opts.Root and reports debounced batches of
// changes until ctx ends, then closes the channel. The baseline is scanned
// before Watch returns, so a change made after it returns is never missed.
// One goroutine runs, and it stops with ctx.
func Watch(ctx context.Context, opts Options) (<-chan Change, error) {
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = DefaultMaxFiles
	}
	rules := DefaultRules()
	if opts.Rules != nil {
		rules = *opts.Rules
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	previous, err := scan(root, rules, opts.MaxFiles)
	if err != nil {
		return nil, err
	}
	out := make(chan Change)
	go func() {
		defer close(out)
		ticker := time.NewTicker(opts.PollInterval)
		defer ticker.Stop()
		pending := map[string]bool{} // path -> envOnly
		failing := false
		var lastChange time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			current, err := scan(root, rules, opts.MaxFiles)
			if err != nil {
				// The tree vanished or grew past the bound; keep the
				// last good baseline and try again. Say so once, not on
				// every poll.
				if !failing && opts.Logf != nil {
					opts.Logf("file watcher cannot scan %s, changes are not seen until it can: %v", root, err)
				}
				failing = true
				continue
			}
			if failing && opts.Logf != nil {
				opts.Logf("file watcher is scanning %s again", root)
			}
			failing = false
			for name, state := range diff(previous, current) {
				pending[name] = state
				lastChange = time.Now()
			}
			previous = current
			if len(pending) == 0 || time.Since(lastChange) < opts.Debounce {
				continue
			}
			change := summarize(pending)
			clear(pending)
			select {
			case out <- change:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func scan(root string, rules Rules, maxFiles int) (map[string]fileState, error) {
	files := make(map[string]fileState)
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			if name == root {
				return err
			}
			return nil // vanished between listing and stat
		}
		if entry.IsDir() {
			if name != root && rules.skipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return nil //nolint:nilerr // a path that cannot be made relative is not watched; the next poll tries again
		}
		rel = filepath.ToSlash(rel)
		watched, envOnly := rules.classify(rel)
		if !watched {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil //nolint:nilerr // a file that vanished between the listing and the stat is simply not watched
		}
		if len(files) >= maxFiles {
			return fmt.Errorf("%w: more than %d files under %s", ErrTooManyFiles, maxFiles, root)
		}
		files[rel] = fileState{modTime: info.ModTime(), size: info.Size(), envOnly: envOnly}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return files, err
}

// diff returns the paths added, removed or modified between two scans, with
// whether each is an env file.
func diff(previous, current map[string]fileState) map[string]bool {
	changed := make(map[string]bool)
	for name, state := range current {
		before, ok := previous[name]
		if !ok || !before.modTime.Equal(state.modTime) || before.size != state.size {
			changed[name] = state.envOnly
		}
	}
	for name, state := range previous {
		if _, ok := current[name]; !ok {
			changed[name] = state.envOnly
		}
	}
	return changed
}

func summarize(pending map[string]bool) Change {
	names := make([]string, 0, len(pending))
	envOnly := true
	for name, env := range pending {
		names = append(names, name)
		envOnly = envOnly && env
	}
	slices.Sort(names)
	change := Change{Count: len(names), EnvOnly: envOnly}
	change.Paths = names[:min(len(names), maxReportedPaths)]
	return change
}
