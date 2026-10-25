package openapi

import "testing"

// validTagName mirrors the tag names encoding/json accepts: letters, digits
// and a fixed set of punctuation.
func TestValidTagName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"":           false,
		"name":       true,
		"user_id":    true,
		"a-b.c:d":    true,
		"Größe":      true,
		"x y":        true,
		"tab\there":  false,
		`back\slash`: false,
		`say"hi"`:    false,
		"it's":       false,
		"nul\x00":    false,
	} {
		if got := validTagName(name); got != want {
			t.Errorf("validTagName(%q) = %v, want %v", name, got, want)
		}
	}
}
