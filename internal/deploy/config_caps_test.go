package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// config/deploy.yml is capped like Forge-Rust's: a file over 1 MiB and a
// document nested deeper than 100 levels are refused, the second with a line
// number (forge-go#89).
func TestLoadConfigRefusesAFileOverOneMiB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, size int) string {
		path := filepath.Join(dir, name)
		text := strings.TrimLeft(validConfig, "\n")
		text += "# " + strings.Repeat("x", size-len(text)-3) + "\n"
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if _, err := LoadConfig(write("exact.yml", MaxConfigBytes)); err != nil {
		t.Fatalf("a file of exactly 1 MiB: %v", err)
	}
	path := write("over.yml", MaxConfigBytes+1)
	_, err := LoadConfig(path)
	if err == nil || err.Error() != fmt.Sprintf("read deploy config: %s is larger than 1048576 bytes", path) {
		t.Fatalf("a file of 1 MiB + 1: %v", err)
	}
	if _, err := LoadConfig(filepath.Join(dir, "absent.yml")); err == nil || !strings.HasPrefix(err.Error(), "read deploy config: open ") {
		t.Fatalf("a missing file: %v", err)
	}
	if _, err := LoadConfig(dir); err == nil || !strings.HasPrefix(err.Error(), "read deploy config: ") {
		t.Fatalf("a directory: %v", err)
	}
}

func TestParseConfigRefusesNestingDeeperThanOneHundredLevels(t *testing.T) {
	t.Parallel()
	block := func(levels int) string {
		var b strings.Builder
		for i := range levels {
			b.WriteString(strings.Repeat(" ", i) + "k:")
			if i == levels-1 {
				b.WriteString(" 1")
			}
			b.WriteString("\n")
		}
		return b.String()
	}
	flow := func(levels int) string {
		return "a: " + strings.Repeat("[", levels) + "1" + strings.Repeat("]", levels) + "\n"
	}
	wantDepth := func(line int) string {
		return fmt.Sprintf("deploy config: yaml: line %d: exceeded max depth of 100", line)
	}
	for _, tt := range []struct {
		name string
		text string
		want string
	}{
		// n nested block mappings hold a scalar at depth n+1, on line n.
		{"block at the limit decodes (then fails on its unknown key)", block(99), "field k not found"},
		{"block one level past the limit", block(100), wantDepth(100)},
		{"block far past the limit", block(5000), wantDepth(100)},
		// m flow sequences hold the scalar at depth m+2, on line 1.
		{"flow at the limit decodes (then fails on its unknown key)", flow(98), "field a not found"},
		{"flow one level past the limit", flow(99), wantDepth(1)},
		{"flow past yaml.v3's own limit", flow(20000), wantDepth(1)},
		{"block sequences past yaml.v3's own limit", "a:\n" + strings.Repeat("- ", 20000) + "x\n", wantDepth(2)},
		// The scanner gives up at its 10,001st bracket, on the line that holds it.
		{"flow past yaml.v3's own limit, over many lines", "a: [\n" + strings.Repeat("[\n", 20000), wantDepth(10001)},
		{"a hostile document after a comment line", "# c\n" + flow(99), wantDepth(2)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseConfig([]byte(tt.text))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
			if strings.Contains(tt.want, "max depth") && err.Error() != tt.want {
				t.Fatalf("err = %q, want exactly %q", err, tt.want)
			}
		})
	}
}
