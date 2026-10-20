package assets

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"slices"
	"strings"
)

// MaxPins bounds an import map.
const MaxPins = 256

// Pin maps a bare module specifier (what a JavaScript `import` statement
// names) to a logical asset.
type Pin struct {
	Specifier string
	Logical   string
}

// ImportMap is a rendered import map: bare specifiers mapped to digested
// asset URLs, the way Rails loads Turbo and Stimulus without a bundler. Its
// inline <script> is allowed by a Content-Security-Policy hash, never by
// 'unsafe-inline'.
type ImportMap struct {
	body string
	tag  template.HTML
	hash string
	urls []string
}

// NewImportMap resolves pins against set. A pin to an unknown asset, a
// duplicate or an invalid specifier is an error, so mistakes stop startup.
func NewImportMap(set *Set, pins ...Pin) (*ImportMap, error) {
	if set == nil {
		return nil, errors.New("assets: an asset set is required")
	}
	if len(pins) > MaxPins {
		return nil, fmt.Errorf("assets: an import map has at most %d pins", MaxPins)
	}
	imports := make(map[string]string, len(pins))
	for _, pin := range pins {
		if pin.Specifier == "" || len(pin.Specifier) > 256 || strings.ContainsFunc(pin.Specifier, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return nil, fmt.Errorf("assets: invalid module specifier %q", pin.Specifier)
		}
		url, ok := set.Path(pin.Logical)
		if !ok {
			return nil, fmt.Errorf("assets: module %q is pinned to unknown asset %q", pin.Specifier, pin.Logical)
		}
		if _, duplicate := imports[pin.Specifier]; duplicate {
			return nil, fmt.Errorf("assets: module specifier %q is pinned more than once", pin.Specifier)
		}
		imports[pin.Specifier] = url
	}
	// json.Marshal sorts map keys and escapes <, > and & (and U+2028/9) as
	// \uXXXX, which is equivalent inside JSON strings and cannot end the
	// <script> element or open an HTML comment.
	encoded, err := json.Marshal(map[string]map[string]string{"imports": imports})
	if err != nil {
		return nil, fmt.Errorf("assets: encode import map: %w", err)
	}
	body := string(encoded)
	sum := sha256.Sum256(encoded)
	urls := make([]string, 0, len(imports))
	for _, url := range imports {
		urls = append(urls, url)
	}
	slices.Sort(urls)
	urls = slices.Compact(urls)
	return &ImportMap{
		body: body,
		// #nosec G203 -- body is json.Marshal output, which escapes <, > and &.
		tag:  template.HTML(`<script type="importmap">` + body + `</script>`),
		hash: base64.StdEncoding.EncodeToString(sum[:]),
		urls: urls,
	}, nil
}

// PinAllFrom pins every .js and .mjs asset of set below dir (a logical
// directory such as "controllers") by its logical name without the
// extension: "controllers/hello_controller.js" becomes the specifier
// "controllers/hello_controller", and "controllers/index.js" is also pinned
// as "controllers". It is Rails' pin_all_from.
func PinAllFrom(set *Set, dir string) []Pin {
	dir = strings.Trim(dir, "/")
	var pins []Pin
	for _, logical := range set.Logicals() {
		rest, ok := strings.CutPrefix(logical, dir+"/")
		if !ok {
			continue
		}
		var specifier string
		switch {
		case strings.HasSuffix(rest, ".js"):
			specifier = strings.TrimSuffix(logical, ".js")
		case strings.HasSuffix(rest, ".mjs"):
			specifier = strings.TrimSuffix(logical, ".mjs")
		default:
			continue
		}
		pins = append(pins, Pin{Specifier: specifier, Logical: logical})
		if rest == "index.js" || rest == "index.mjs" {
			pins = append(pins, Pin{Specifier: dir, Logical: logical})
		}
	}
	return pins
}

// JSON is the exact text between the <script> tags.
func (m *ImportMap) JSON() string { return m.body }

// Tag is the complete <script type="importmap"> element. It has no nonce: it
// is allowed by hash. It must come before any module script, and its bytes
// must not be reformatted, because the hash covers the text between the tags.
func (m *ImportMap) Tag() template.HTML { return m.tag }

// SHA256 is the base64 SHA-256 of JSON.
func (m *ImportMap) SHA256() string { return m.hash }

// CSPSource is the script-src source expression that allows exactly this
// map: 'sha256-<base64>'.
func (m *ImportMap) CSPSource() string { return "'sha256-" + m.hash + "'" }

// URLs returns the distinct digested URL of every pinned module, sorted, for
// <link rel="modulepreload">.
func (m *ImportMap) URLs() []string { return slices.Clone(m.urls) }
