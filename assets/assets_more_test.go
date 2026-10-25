package assets_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rocketinsidecorp/forge-go/assets"
)

// unreadable lists a file but cannot read it.
type unreadable struct{ fstest.MapFS }

func (unreadable) ReadFile(string) ([]byte, error) { return nil, errors.New("disk on fire") }
func (u unreadable) Open(name string) (fs.File, error) {
	if name == "." {
		return u.MapFS.Open(name)
	}
	return nil, errors.New("disk on fire")
}

func TestNewRefusesSourcesItCannotServeSafely(t *testing.T) {
	t.Parallel()
	if _, err := assets.New(assets.Options{}, fstest.MapFS{"link.css": {Mode: fs.ModeSymlink, Data: []byte("target.css")}}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a symbolic link: %v", err)
	}
	if _, err := assets.New(assets.Options{}, unreadable{fstest.MapFS{"app.css": {Data: []byte("body{}")}}}); err == nil || !strings.Contains(err.Error(), "read app.css") {
		t.Errorf("an unreadable file: %v", err)
	}
	broken := fstest.MapFS{"ok.css": {Data: []byte("a{}")}}
	set, err := assets.New(assets.Options{Prefix: "/static"}, broken)
	if err != nil {
		t.Fatal(err)
	}
	if set.Prefix() != "/static" {
		t.Errorf("Prefix = %q", set.Prefix())
	}
	if path, ok := set.Path("missing.css"); ok || path != "" {
		t.Errorf("Path of an unknown asset = %q, %v", path, ok)
	}
	if digest, ok := set.Digest("missing.css"); ok || digest != "" {
		t.Errorf("Digest of an unknown asset = %q, %v", digest, ok)
	}
	if digest, ok := set.Digest("ok.css"); !ok || len(digest) != 64 {
		t.Errorf("Digest = %q, %v", digest, ok)
	}
}
