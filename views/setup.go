package views

import (
	"errors"
	"fmt"
	"html/template"
	"regexp"
	"strings"

	"github.com/rocketinsidecorp/forge-go/assets"
	"github.com/rocketinsidecorp/forge-go/broadcast"
	"github.com/rocketinsidecorp/forge-go/i18n"
	"github.com/rocketinsidecorp/forge-go/web"
)

// setupError reports a helper used without the Option that enables it. Load
// fails on it instead of leaving the mistake for the first request.
type setupError struct{ message string }

func (e *setupError) Error() string { return e.message }

var styleHashPattern = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)

// defaultStreamsPath is where applications mount broadcast.NewHandler.
const defaultStreamsPath = "/streams"

// setup is what the asset and stream helpers need, resolved once by Load.
type setup struct {
	assets      *assets.Set
	importMap   *assets.ImportMap
	streams     *broadcast.Signer
	streamsPath string
	policy      string
	translator  *i18n.Translator
}

func newSetup(opts Options) (*setup, error) {
	result := &setup{assets: opts.Assets, importMap: opts.ImportMap, streams: opts.Streams, streamsPath: opts.StreamsPath, translator: opts.I18n}
	if result.importMap != nil && result.assets == nil {
		return nil, errors.New("views: Options.ImportMap needs Options.Assets")
	}
	if result.streamsPath == "" {
		result.streamsPath = defaultStreamsPath
	}
	if !web.IsLocalPath(result.streamsPath) || strings.ContainsAny(result.streamsPath, "?#") {
		return nil, fmt.Errorf("views: invalid streams path %q", result.streamsPath)
	}
	scripts, styles := "script-src 'self'", "style-src 'self'"
	if result.importMap != nil {
		scripts += " " + result.importMap.CSPSource()
	}
	for _, hash := range opts.StyleHashes {
		if !styleHashPattern.MatchString(hash) {
			return nil, fmt.Errorf("views: style hash %q is not a base64 SHA-256 digest", hash)
		}
		styles += " 'sha256-" + hash + "'"
	}
	result.policy = strings.NewReplacer("script-src 'self'", scripts, "style-src 'self'", styles).Replace(DefaultContentSecurityPolicy)
	return result, nil
}

func (s *setup) needAssets(helper string) error {
	if s.assets == nil {
		return &setupError{helper + " needs views.Options.Assets"}
	}
	return nil
}

func (s *setup) needStreams(helper string) error {
	if s.streams == nil {
		return &setupError{helper + " needs views.Options.Streams"}
	}
	return nil
}

// assetPath returns the digested URL path of a logical asset name.
func (s *setup) assetPath(logical string) (string, error) {
	if err := s.needAssets("assetPath"); err != nil {
		return "", err
	}
	path, ok := s.assets.Path(logical)
	if !ok {
		return "", &setupError{fmt.Sprintf("assetPath: unknown asset %q", logical)}
	}
	return path, nil
}

// stylesheetTag returns <link rel="stylesheet"> for a logical asset; ".css"
// is added when the name has no extension.
func (s *setup) stylesheetTag(name string) (template.HTML, error) {
	if err := s.needAssets("stylesheetTag"); err != nil {
		return "", err
	}
	path, err := s.assetPath(withExtension(name, ".css"))
	if err != nil {
		return "", err
	}
	// #nosec G203 -- constant markup around a digested asset path (letters, digits and . _ - /), escaped again here.
	return template.HTML(`<link rel="stylesheet" href="` + template.HTMLEscapeString(path) + `">`), nil
}

// javascriptImportmapTags returns the import map, a modulepreload link for
// each pinned module, and the module script for the entry asset (".js" is
// added when the name has no extension). The import map goes first, as
// browsers require.
func (s *setup) javascriptImportmapTags(entry string) (template.HTML, error) {
	if err := s.needAssets("javascriptImportmapTags"); err != nil {
		return "", err
	}
	if s.importMap == nil {
		return "", &setupError{"javascriptImportmapTags needs views.Options.ImportMap"}
	}
	path, err := s.assetPath(withExtension(entry, ".js"))
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString(string(s.importMap.Tag()))
	for _, url := range s.importMap.URLs() {
		out.WriteString(`<link rel="modulepreload" href="` + template.HTMLEscapeString(url) + `">`)
	}
	out.WriteString(`<script type="module" src="` + template.HTMLEscapeString(path) + `"></script>`)
	// #nosec G203 -- the import map tag is json.Marshal output; the rest is constant markup around digested asset paths, escaped again here.
	return template.HTML(out.String()), nil
}

// streamSource returns <turbo-stream-source> for a signed channel.
func (s *setup) streamSource(channel broadcast.Channel) (template.HTML, error) {
	source, err := s.streams.SourceURL(s.streamsPath, channel)
	if err != nil {
		return "", err
	}
	// #nosec G203 -- constant markup around a URL of a local path and a base64url token, escaped again here.
	return template.HTML(`<turbo-stream-source src="` + template.HTMLEscapeString(source) + `"></turbo-stream-source>`), nil
}

func withExtension(name, extension string) string {
	if strings.Contains(name, ".") {
		return name
	}
	return name + extension
}
