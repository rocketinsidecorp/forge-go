package scaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// withChecksum stamps text (which carries the placeholder) with its checksum,
// as RenderRoutes does.
func withChecksum(text string) []byte {
	sum := sha256.Sum256([]byte(text))
	return []byte(strings.Replace(text, checksumPrefix+checksumPlaceholder, checksumPrefix+hex.EncodeToString(sum[:]), 1))
}

// A file whose checksum matches but whose structure is not the generator's
// is refused too.
func TestParseRoutesRefusesAStructureItDidNotWrite(t *testing.T) {
	t.Parallel()
	head := "//\n" + checksumPrefix + checksumPlaceholder + "\n\npackage bootstrap\n\n"
	for name, body := range map[string]string{
		"no function":          "func other() error {\n\treturn nil\n}\n",
		"text before a marker": "func generatedRoutes() error {\n\tx := 1\n\t// forge:guard x\n\treturn nil\n}\n",
		"no return":            "func generatedRoutes() error {\n\t// forge:guard x\n\tpanic(1)\n}\n",
	} {
		if _, err := ParseRoutes(withChecksum(head + body)); !errors.Is(err, ErrRoutesEdited) {
			t.Errorf("%s: %v", name, err)
		}
	}
	blocks, err := ParseRoutes(withChecksum(head + "func generatedRoutes() error {\n\t// forge:guard x\n\tx := 1\n\treturn nil\n}\n"))
	if err != nil || len(blocks) != 1 || blocks[0].Body != "\tx := 1\n" {
		t.Fatalf("a valid seam: %+v %v", blocks, err)
	}
}
