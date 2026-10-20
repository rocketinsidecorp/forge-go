package hotwire_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/assets"
	"github.com/rocketinsidecorp/forge-go/hotwire"
)

func read(t *testing.T, fsys fs.FS, name string) []byte {
	t.Helper()
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestVendoredFilesMatchTheirPinnedDigests(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		hotwire.TurboAsset:    hotwire.TurboSHA256,
		hotwire.StimulusAsset: hotwire.StimulusSHA256,
	} {
		sum := sha256.Sum256(read(t, hotwire.Files(), name))
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s was modified: sha256 %s, pinned %s", name, got, want)
		}
	}
	if !strings.Contains(string(read(t, hotwire.Files(), hotwire.StimulusAsset)), "export {") ||
		!strings.Contains(string(read(t, hotwire.Files(), hotwire.TurboAsset)), "export {") {
		t.Error("expected ES module builds")
	}
	if !strings.HasPrefix(string(read(t, hotwire.Files(), hotwire.TurboAsset)), "/*!\nTurbo "+hotwire.TurboVersion+"\n") {
		t.Error("turbo.js banner does not match TurboVersion")
	}
}

func TestLicenseNoticesAreRetained(t *testing.T) {
	t.Parallel()
	for name, holder := range map[string]string{
		"vendor/TURBO-LICENSE":    "37signals",
		"vendor/STIMULUS-LICENSE": "Basecamp",
	} {
		text := string(read(t, hotwire.Licenses(), name))
		if !strings.Contains(text, "Permission is hereby granted") || !strings.Contains(text, holder) {
			t.Errorf("%s is not the upstream MIT notice for %s", name, holder)
		}
	}
}

// TestProgressBarStyleHash recomputes the inline <style> Turbo creates from
// its own source, the way its unindent helper builds it, and checks the CSP
// hash pinned for it.
func TestProgressBarStyleHash(t *testing.T) {
	t.Parallel()
	source := string(read(t, hotwire.Files(), hotwire.TurboAsset))
	match := regexp.MustCompile("(?s)static get defaultCSS\\(\\) \\{\\s*return unindent`\\n(.*?)`").FindStringSubmatch(source)
	if match == nil {
		t.Fatal("Turbo's progress bar CSS was not found; update TurboProgressBarStyleSHA256 and this test")
	}
	css := strings.NewReplacer("${ProgressBar.animationDuration}", "300", "${ProgressBar.animationDuration / 2}", "150").Replace(match[1])
	lines := strings.Split(css, "\n")
	indent := len(lines[0]) - len(strings.TrimLeft(lines[0], " \t"))
	for i, line := range lines {
		if len(line) >= indent {
			lines[i] = line[indent:]
		} else {
			lines[i] = ""
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	if got := base64.StdEncoding.EncodeToString(sum[:]); got != hotwire.TurboProgressBarStyleSHA256 {
		t.Fatalf("Turbo's inline style hashes to %s, pinned %s", got, hotwire.TurboProgressBarStyleSHA256)
	}
	if got := hotwire.StyleHashes(); len(got) != 1 || got[0] != hotwire.TurboProgressBarStyleSHA256 {
		t.Fatalf("StyleHashes = %v", got)
	}
}

func TestServedAsDigestedModules(t *testing.T) {
	t.Parallel()
	set, err := assets.New(assets.Options{}, hotwire.Files())
	if err != nil {
		t.Fatal(err)
	}
	if turbo, _ := set.Path(hotwire.TurboAsset); turbo != "/assets/vendor/turbo-b9d35d123a07614f.js" {
		t.Errorf("turbo path = %q", turbo)
	}
	if stimulus, _ := set.Path(hotwire.StimulusAsset); stimulus != "/assets/vendor/stimulus-23deecdac6f36c08.js" {
		t.Errorf("stimulus path = %q", stimulus)
	}
	imports, err := assets.NewImportMap(set, hotwire.Pins()...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(imports.JSON(), `"@hotwired/stimulus":"/assets/vendor/stimulus-23deecdac6f36c08.js"`) {
		t.Errorf("import map = %s", imports.JSON())
	}
	url, _ := set.Path(hotwire.TurboAsset)
	recorder := httptest.NewRecorder()
	set.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, url, nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || !strings.HasPrefix(recorder.Body.String(), "/*!\nTurbo") {
		t.Fatalf("status=%d headers=%v", recorder.Code, recorder.Header())
	}
}
