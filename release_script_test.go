package forge_test

// Tests of scripts/release-build.sh and scripts/release-checksums.sh, which
// the release workflow runs on a version tag: the archive names a release
// publishes, and, on the host platform, the archive contents, the version the
// binary reports, byte-for-byte reproducibility and the checksums file.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// releaseScript runs a release script from the repository root with extra
// environment variables, returning its output.
func releaseScript(t *testing.T, env []string, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", append([]string{filepath.Join("scripts", name)}, args...)...)
	command.Env = append(os.Environ(), env...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func requireReleaseTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the release scripts are bash scripts for a Unix runner")
	}
	for _, tool := range []string{"bash", "git", "go", "gzip", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
}

func TestReleaseBuildDryRunListsTheArchiveNames(t *testing.T) {
	t.Parallel()
	requireReleaseTools(t)
	output, err := releaseScript(t, []string{"DRY_RUN=1"}, "release-build.sh", "v0.1.0", filepath.Join(t.TempDir(), "never-created"))
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, output)
	}
	want := []string{
		"forge_v0.1.0_linux_amd64.tar.gz", "forge-proxy_v0.1.0_linux_amd64.tar.gz",
		"forge_v0.1.0_linux_arm64.tar.gz", "forge-proxy_v0.1.0_linux_arm64.tar.gz",
		"forge_v0.1.0_darwin_amd64.tar.gz", "forge-proxy_v0.1.0_darwin_amd64.tar.gz",
		"forge_v0.1.0_darwin_arm64.tar.gz", "forge-proxy_v0.1.0_darwin_arm64.tar.gz",
		"forge_v0.1.0_windows_amd64.zip", "forge-proxy_v0.1.0_windows_amd64.zip",
		"forge_v0.1.0_windows_arm64.zip", "forge-proxy_v0.1.0_windows_arm64.zip",
	}
	if got := strings.Fields(output); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("archive names:\n got %v\nwant %v", got, want)
	}
}

func TestReleaseBuildRefusesAnythingButAVersion(t *testing.T) {
	t.Parallel()
	requireReleaseTools(t)
	for _, version := range []string{"main", "0.1.0", "v1", "v1.2", "v1.2.3;rm", "v1.2.3 x", "$(id)"} {
		output, err := releaseScript(t, []string{"DRY_RUN=1"}, "release-build.sh", version, t.TempDir())
		if err == nil {
			t.Errorf("%q was accepted: %s", version, output)
		}
	}
	for _, version := range []string{"v0.1.0", "v1.2.3-rc.1", "v10.20.30"} {
		if output, err := releaseScript(t, []string{"DRY_RUN=1"}, "release-build.sh", version, t.TempDir()); err != nil {
			t.Errorf("%q was refused: %v\n%s", version, err, output)
		}
	}
}

// archiveFile is one entry of a release archive.
type archiveFile struct {
	mode int64
	data []byte
}

func readTarGz(t *testing.T, path string) map[string]archiveFile {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	if compressed.Name != "" || !compressed.ModTime.IsZero() {
		t.Errorf("%s: the gzip header carries a name %q or time %v", path, compressed.Name, compressed.ModTime)
	}
	files := map[string]archiveFile{}
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			t.Errorf("%s: %s has an owner (%d:%d %q:%q)", path, header.Name, header.Uid, header.Gid, header.Uname, header.Gname)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = archiveFile{mode: header.Mode, data: data}
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// TestReleaseBuildProducesReproducibleArchivesForTheHost builds the host
// platform's archives like the release does, then checks their names, their
// contents, that the extracted binaries report the injected version and the
// commit, that a second build is byte-identical, and the checksums file.
func TestReleaseBuildProducesReproducibleArchivesForTheHost(t *testing.T) {
	if testing.Short() {
		t.Skip("builds both binaries twice")
	}
	requireReleaseTools(t)
	platform := runtime.GOOS + "/" + runtime.GOARCH
	switch platform {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64":
	default:
		t.Skipf("no release archive for %s", platform)
	}
	if output, err := exec.Command("tar", "--version").CombinedOutput(); err != nil || !strings.Contains(string(output), "GNU tar") {
		if _, lookErr := exec.LookPath("gtar"); lookErr != nil {
			t.Skip("GNU tar is not installed")
		}
	}
	head, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	commit := strings.TrimSpace(string(head))[:12]
	const version = "v0.0.0-test.1"
	env := []string{"TARGETS=" + platform}

	first := t.TempDir()
	if output, err := releaseScript(t, env, "release-build.sh", version, first); err != nil {
		t.Fatalf("release-build.sh: %v\n%s", err, output)
	}
	var names []string
	entries, err := os.ReadDir(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	osArch := runtime.GOOS + "_" + runtime.GOARCH
	want := []string{"forge-proxy_" + version + "_" + osArch + ".tar.gz", "forge_" + version + "_" + osArch + ".tar.gz"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("archives %v, want %v", names, want)
	}

	for _, command := range []string{"forge", "forge-proxy"} {
		base := command + "_" + version + "_" + osArch
		files := readTarGz(t, filepath.Join(first, base+".tar.gz"))
		if len(files) != 4 { // the directory entry and three files
			t.Errorf("%s: %d entries, want the directory, the binary, LICENSE and README.md", base, len(files))
		}
		binary, ok := files[base+"/"+command]
		if !ok || binary.mode&0o755 != 0o755 || len(binary.data) < 1<<20 {
			t.Fatalf("%s: no executable %s (found %v)", base, command, ok)
		}
		license, readme := files[base+"/LICENSE"], files[base+"/README.md"]
		if license.mode != 0o644 || !bytes.Equal(license.data, []byte(readText(t, "LICENSE"))) ||
			readme.mode != 0o644 || !bytes.Equal(readme.data, []byte(readText(t, "README.md"))) {
			t.Errorf("%s: LICENSE or README.md missing, different from the repository's, or with the wrong mode", base)
		}
		// Run the extracted binary: it reports the version the tag names.
		executable := filepath.Join(t.TempDir(), command)
		if err := os.WriteFile(executable, binary.data, 0o700); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command(executable, "version").CombinedOutput()
		if err != nil {
			t.Fatalf("%s version: %v\n%s", command, err, output)
		}
		line := strings.TrimSpace(string(output))
		for _, part := range []string{command + " " + version + " (", commit, "go1.", " " + runtime.GOOS + "/" + runtime.GOARCH + ")"} {
			if !strings.Contains(line, part) {
				t.Errorf("%s version = %q, want it to contain %q", command, line, part)
			}
		}
	}

	// The same commit and toolchain give the same bytes.
	second := t.TempDir()
	if output, err := releaseScript(t, env, "release-build.sh", version, second); err != nil {
		t.Fatalf("second release-build.sh: %v\n%s", err, output)
	}
	for _, name := range want {
		if a, b := fileSHA256(t, filepath.Join(first, name)), fileSHA256(t, filepath.Join(second, name)); a != b {
			t.Errorf("%s is not reproducible: %s != %s", name, a, b)
		}
	}

	// checksums.txt lists every file, in sha256sum format, and the script
	// verifies it.
	if output, err := releaseScript(t, nil, "release-checksums.sh", version, first); err != nil {
		t.Fatalf("release-checksums.sh: %v\n%s", err, output)
	}
	checksums, err := os.ReadFile(filepath.Join(first, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, name := range want {
		lines = append(lines, fileSHA256(t, filepath.Join(first, name))+"  "+name)
	}
	if got := strings.TrimSpace(string(checksums)); got != strings.Join(lines, "\n") {
		t.Errorf("checksums.txt:\n%s\nwant\n%s", got, strings.Join(lines, "\n"))
	}
}

// TestReleaseChecksumsRenamesTheSBOMsAndRefusesAnEmptyRelease uses small
// files, no build.
func TestReleaseChecksumsRenamesTheSBOMsAndRefusesAnEmptyRelease(t *testing.T) {
	t.Parallel()
	requireReleaseTools(t)
	release, sbom := t.TempDir(), t.TempDir()
	if output, err := releaseScript(t, nil, "release-checksums.sh", "v1.2.3", release); err == nil {
		t.Errorf("an empty release directory was accepted: %s", output)
	}
	if err := os.WriteFile(filepath.Join(release, "forge_v1.2.3_linux_amd64.tar.gz"), []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := releaseScript(t, nil, "release-checksums.sh", "v1.2.3", release, sbom); err == nil {
		t.Errorf("a release without SBOMs was accepted: %s", output)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		dir := filepath.Join(sbom, "sbom-v1.2.3-linux-"+arch)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, command := range []string{"forge", "forge-proxy"} {
			if err := os.WriteFile(filepath.Join(dir, command+".cdx.json"), []byte(`{"arch":"`+arch+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if output, err := releaseScript(t, nil, "release-checksums.sh", "v1.2.3", release, sbom); err != nil {
		t.Fatalf("release-checksums.sh: %v\n%s", err, output)
	}
	checksums, err := os.ReadFile(filepath.Join(release, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"forge_v1.2.3_linux_amd64.tar.gz",
		"forge_v1.2.3_linux_amd64.cdx.json", "forge_v1.2.3_linux_arm64.cdx.json",
		"forge-proxy_v1.2.3_linux_amd64.cdx.json", "forge-proxy_v1.2.3_linux_arm64.cdx.json",
	} {
		if !strings.Contains(string(checksums), "  "+name+"\n") {
			t.Errorf("checksums.txt does not list %s:\n%s", name, checksums)
		}
	}
	if strings.Contains(string(checksums), "checksums.txt") {
		t.Errorf("checksums.txt lists itself:\n%s", checksums)
	}
	// Running it again does not list the previous checksums file.
	if output, err := releaseScript(t, nil, "release-checksums.sh", "v1.2.3", release); err != nil {
		t.Fatalf("second release-checksums.sh: %v\n%s", err, output)
	}
}

// TestReleaseBuildRefusesADirtyTreeOfATag: a release is built from exactly the
// tagged commit. In a scratch repository, a change to a tracked file makes the
// build of the tag's version fail before anything is built, while an untagged
// version is let through (and would be stamped -dirty).
func TestReleaseBuildRefusesADirtyTreeOfATag(t *testing.T) {
	t.Parallel()
	requireReleaseTools(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join("scripts", "release-build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		filepath.Join("scripts", "release-build.sh"): script,
		"go.mod":    []byte("module example.com/x\n\ngo 1.27\n"),
		"LICENSE":   []byte("MIT\n"),
		"README.md": []byte("# x\n"),
	} {
		if err := os.WriteFile(filepath.Join(repo, name), content, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "x")
	git("tag", "v1.0.0")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(version string) (string, error) {
		command := exec.Command("bash", filepath.Join(repo, "scripts", "release-build.sh"), version, filepath.Join(t.TempDir(), "out"))
		command.Dir = repo
		command.Env = append(os.Environ(), "TARGETS=linux/amd64")
		output, err := command.CombinedOutput()
		return string(output), err
	}
	if output, err := run("v1.0.0"); err == nil || !strings.Contains(output, "working tree has changes") {
		t.Errorf("a dirty tree of the tag was built: %v\n%s", err, output)
	}
	// An untagged version passes the check; the scratch module has no cmd/forge,
	// so the build itself fails, after the check.
	if output, err := run("v9.9.9"); err == nil || strings.Contains(output, "working tree has changes") {
		t.Errorf("an untagged local build was refused as a release: %v\n%s", err, output)
	}
}

// TestReleaseDraftStopsOnAnyProblemAndNeverPublishes runs release-draft.sh with
// a fake gh: a failing build script, a missing asset and a bad checksum stop it
// before gh runs; a complete release calls `gh release create --draft`.
func TestReleaseDraftStopsOnAnyProblemAndNeverPublishes(t *testing.T) {
	t.Parallel()
	requireReleaseTools(t)
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	fake := "#!/bin/sh\necho \"$@\" >> " + calls + "\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GH_REPO=example/forge", "GITHUB_REPOSITORY="}
	called := func() string {
		content, _ := os.ReadFile(calls)
		return string(content)
	}
	release := t.TempDir()
	if output, err := releaseScript(t, env, "release-draft.sh", "not-a-version", release); err == nil || !strings.Contains(output, "refused") {
		t.Errorf("a failing build script was swallowed: %v\n%s", err, output)
	}
	if output, err := releaseScript(t, env, "release-draft.sh", "v1.2.3", release); err == nil || !strings.Contains(output, "is missing") {
		t.Errorf("a release without assets was accepted: %v\n%s", err, output)
	}
	names, err := releaseScript(t, []string{"DRY_RUN=1"}, "release-build.sh", "v1.2.3", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, name := range strings.Fields(names) {
		if err := os.WriteFile(filepath.Join(release, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(name))
		lines = append(lines, hex.EncodeToString(sum[:])+"  "+name)
	}
	if err := os.WriteFile(filepath.Join(release, "checksums.txt.sigstore.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	checksums := filepath.Join(release, "checksums.txt")
	if err := os.WriteFile(checksums, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A tampered archive fails the checksum check.
	tampered := filepath.Join(release, strings.Fields(names)[0])
	if err := os.WriteFile(tampered, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := releaseScript(t, env, "release-draft.sh", "v1.2.3", release); err == nil {
		t.Errorf("a tampered archive was drafted: %s", output)
	}
	if called() != "" {
		t.Errorf("gh ran before the checks passed: %s", called())
	}
	if err := os.WriteFile(tampered, []byte(strings.Fields(names)[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := releaseScript(t, env, "release-draft.sh", "v1.2.3", release); err != nil {
		t.Fatalf("a complete release was refused: %v\n%s", err, output)
	}
	got := called()
	for _, want := range []string{"release create v1.2.3", "--draft", "--verify-tag", "--generate-notes", "--repo example/forge", "checksums.txt.sigstore.json"} {
		if !strings.Contains(got, want) {
			t.Errorf("gh was called with %q, want it to contain %q", got, want)
		}
	}
	for _, refused := range []string{"--prerelease", "release edit", "--latest"} {
		if strings.Contains(got, refused) {
			t.Errorf("gh was called with %q", got)
		}
	}
}
