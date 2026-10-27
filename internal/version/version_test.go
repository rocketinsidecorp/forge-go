package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestDescribeShowsVersionCommitDateAndToolchain(t *testing.T) {
	t.Parallel()
	const commit = "3f2a9c1d07be5a6c4d8e1f0a9b8c7d6e5f4a3b2c"
	got := describe("forge", "v0.1.0", commit, "2026-10-27T12:00:00Z", nil, "go1.27.3", "linux", "amd64")
	want := "forge v0.1.0 (3f2a9c1d07be, 2026-10-27T12:00:00Z, go1.27.3 linux/amd64)"
	if got != want {
		t.Errorf("describe = %q, want %q", got, want)
	}
}

func TestDescribeReadsTheVersionControlInformationOfAnUnstampedBuild(t *testing.T) {
	t.Parallel()
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
		{Key: "vcs.time", Value: "2026-10-01T00:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}}
	got := describe("forge-proxy", "dev", "unknown", "unknown", info, "go1.27.3", "darwin", "arm64")
	want := "forge-proxy dev (0123456789ab-dirty, 2026-10-01T00:00:00Z, go1.27.3 darwin/arm64)"
	if got != want {
		t.Errorf("describe = %q, want %q", got, want)
	}
	// What the linker stamped wins over what the toolchain recorded.
	got = describe("forge", "v0.1.0", "aaaaaaaaaaaa", "2026-01-01T00:00:00Z", info, "go1.27.3", "linux", "amd64")
	if !strings.Contains(got, "(aaaaaaaaaaaa, 2026-01-01T00:00:00Z,") {
		t.Errorf("a stamped build was overridden: %q", got)
	}
}

func TestDescribeStaysUnknownWithoutAnySource(t *testing.T) {
	t.Parallel()
	got := describe("forge", "dev", "unknown", "unknown", &debug.BuildInfo{}, "go1.27.3", "linux", "amd64")
	if want := "forge dev (unknown, unknown, go1.27.3 linux/amd64)"; got != want {
		t.Errorf("describe = %q, want %q", got, want)
	}
	if line := Describe("forge", "dev"); !strings.HasPrefix(line, "forge dev (") {
		t.Errorf("Describe = %q", line)
	}
}
