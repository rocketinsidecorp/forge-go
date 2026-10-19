// Package assets serves an application's embedded static files (stylesheets,
// scripts, images, fonts) under content-digested, immutable URLs, like the
// Rails asset pipeline without a build step: app.css is served as
// /assets/app-<digest>.css, so the response can be cached forever and a
// changed file gets a new URL.
//
// The table is built once at startup from one or more fs.FS values (usually
// embed.FS), so a missing or malformed file stops the application instead of
// producing a broken link at request time. The handler serves only the
// entries of that table: there is no directory listing, no path traversal and
// no way to reach a file that was not embedded on purpose.
//
//	set, err := assets.New(assets.Options{}, appAssets, javascript.Files(), hotwire.Files())
//	...
//	app.Handle("GET /assets/", set.Handler())
//	url, _ := set.Path("application.css") // /assets/application-0123456789abcdef.css
package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"
)

// DefaultPrefix is the URL prefix assets are served under.
const DefaultPrefix = "/assets"

// CacheControl is sent with every asset. The URL contains the content digest,
// so a response never goes stale.
const CacheControl = "public, max-age=31536000, immutable"

// digestChars is the number of hex characters of the SHA-256 digest placed in
// a served file name (64 bits: enough to bust caches, short enough to read).
// The ETag carries the full digest. The sibling Rust framework uses the same
// length, so both produce identical URLs for identical files.
const digestChars = 16

// svgPolicy locks down an SVG opened directly (not through <img>): no
// scripts, no network, no framing.
const svgPolicy = "default-src 'none'; style-src 'unsafe-inline'; sandbox"

// Options configures New.
type Options struct {
	// Prefix is the URL path assets are served under: an absolute path of
	// safe segments without a trailing slash. Empty means DefaultPrefix.
	Prefix string
}

// Set is an immutable, concurrency-safe table of digested assets.
type Set struct {
	prefix    string
	logical   map[string]*asset
	served    map[string]*asset
	logicalOf []string
}

type asset struct {
	logical     string
	served      string // path below the prefix, with the digest
	url         string
	digest      string // full SHA-256, lowercase hex
	etag        string
	contentType string
	svg         bool
	body        []byte
}

// New reads every file of sources into a table. Logical names are the paths
// inside each source ("css/app.css"); the same logical name in two sources is
// an error. Directories and files whose name starts with "." are skipped; any
// other file must have an extension Forge serves (css, js, mjs, map, svg,
// png, jpg, jpeg, gif, webp, ico, woff2, txt) and a name made of letters,
// digits, '.', '_' and '-', so embed only what should be public.
func New(opts Options, sources ...fs.FS) (*Set, error) {
	prefix := opts.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if !validPrefix(prefix) {
		return nil, fmt.Errorf("assets: invalid prefix %q: use an absolute path such as %q", prefix, DefaultPrefix)
	}
	set := &Set{prefix: prefix, logical: map[string]*asset{}, served: map[string]*asset{}}
	for _, source := range sources {
		if source == nil {
			return nil, errors.New("assets: a source filesystem is nil")
		}
		if err := set.add(source); err != nil {
			return nil, err
		}
	}
	slices.Sort(set.logicalOf)
	return set, nil
}

func (s *Set) add(source fs.FS) error {
	return fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name != "." && strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("assets: %s is not a regular file", name)
		}
		body, err := fs.ReadFile(source, name)
		if err != nil {
			return fmt.Errorf("assets: read %s: %w", name, err)
		}
		return s.insert(name, body)
	})
}

func (s *Set) insert(logical string, body []byte) error {
	if !validLogical(logical) {
		return fmt.Errorf("assets: invalid asset name %q: use letters, digits, '.', '_' and '-' in each path segment", logical)
	}
	extension := strings.ToLower(path.Ext(logical))
	contentType, ok := contentTypes[extension]
	if !ok {
		return fmt.Errorf("assets: %s has an extension Forge does not serve", logical)
	}
	if _, duplicate := s.logical[logical]; duplicate {
		return fmt.Errorf("assets: %s appears in more than one source", logical)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	served := strings.TrimSuffix(logical, path.Ext(logical)) + "-" + digest[:digestChars] + path.Ext(logical)
	entry := &asset{
		logical:     logical,
		served:      served,
		url:         s.prefix + "/" + served,
		digest:      digest,
		etag:        `"` + digest + `"`,
		contentType: contentType,
		svg:         extension == ".svg",
		body:        body,
	}
	s.logical[logical] = entry
	s.served[served] = entry
	s.logicalOf = append(s.logicalOf, logical)
	return nil
}

// Prefix is the URL path the assets are served under.
func (s *Set) Prefix() string { return s.prefix }

// Len is the number of assets.
func (s *Set) Len() int { return len(s.logical) }

// Logicals returns the logical names, sorted.
func (s *Set) Logicals() []string { return slices.Clone(s.logicalOf) }

// Path returns the digested URL path of a logical name, such as
// /assets/application-0123456789abcdef.css for "application.css". It reports
// false for names that are not in the table, so a typo can never produce an
// undigested or broken link.
func (s *Set) Path(logical string) (string, bool) {
	entry, ok := s.logical[logical]
	if !ok {
		return "", false
	}
	return entry.url, true
}

// Digest returns the full SHA-256 of an asset as lowercase hex.
func (s *Set) Digest(logical string) (string, bool) {
	entry, ok := s.logical[logical]
	if !ok {
		return "", false
	}
	return entry.digest, true
}

// Handler serves the table under the prefix; mount it with a subtree
// pattern such as "GET /assets/". It answers GET and HEAD for exact digested
// names only (everything else is 404: undigested names, "..", encoded
// separators, directories), with the correct Content-Type, nosniff, an ETag
// and `Cache-Control: public, max-age=31536000, immutable`. A matching
// If-None-Match gets 304.
func (s *Set) Handler() http.Handler {
	return http.HandlerFunc(s.serve)
}

func (s *Set) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	// RawPath is set only when the request used an escape the default
	// encoding would not (%2F, %2e): such paths never name an asset.
	below, ok := strings.CutPrefix(r.URL.Path, s.prefix+"/")
	var entry *asset
	if ok && r.URL.RawPath == "" {
		entry = s.served[below]
	}
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	header := w.Header()
	header.Set("Content-Type", entry.contentType)
	header.Set("Cache-Control", CacheControl)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("ETag", entry.etag)
	if entry.svg {
		header.Set("Content-Security-Policy", svgPolicy)
	}
	// ServeContent answers conditional requests, HEAD and ranges. The zero
	// modification time sends no Last-Modified: the digest is the validator.
	http.ServeContent(w, r, entry.served, time.Time{}, bytes.NewReader(entry.body))
}

var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
}

func validPrefix(prefix string) bool {
	rest, ok := strings.CutPrefix(prefix, "/")
	if !ok || rest == "" {
		return false
	}
	for segment := range strings.SplitSeq(rest, "/") {
		if !validSegment(segment) {
			return false
		}
	}
	return true
}

func validLogical(logical string) bool {
	for segment := range strings.SplitSeq(logical, "/") {
		if !validSegment(segment) {
			return false
		}
	}
	return logical != ""
}

// validSegment allows ASCII letters, digits, '.', '_' and '-', not starting
// with a dot: nothing that needs URL escaping or could be read as route
// syntax.
func validSegment(segment string) bool {
	if segment == "" || segment[0] == '.' {
		return false
	}
	for i := range len(segment) {
		if !segmentByte(segment[i]) {
			return false
		}
	}
	return true
}

func segmentByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return c == '.' || c == '_' || c == '-'
}
