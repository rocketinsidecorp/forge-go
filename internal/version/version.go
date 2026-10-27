// Package version holds the build information of the forge and forge-proxy
// commands and renders it for `forge version` and `forge-proxy version`.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Build information, set at link time (-ldflags -X) by scripts/release-build.sh
// and "dev" or "unknown" in a plain go build.
var (
	// Version is the release version of the forge command.
	Version = "dev"
	// Commit is the source revision the command was built from.
	Commit = "unknown"
	// Date is the UTC time of that revision (a release stamps the commit's
	// time, not the clock, so a rebuild of the tag is identical).
	Date = "unknown"
)

// shortCommit is how many characters of a revision Describe shows, as many
// as `forge deploy` uses for an image tag.
const shortCommit = 12

// Describe is the one-line description of this build:
//
//	forge v0.1.0 (3f2a9c1d07be, 2026-10-27T12:00:00Z, go1.27.3 linux/amd64)
//
// release is the version the caller resolved (it may know more than the
// linker did). A commit or date the linker did not stamp is read from the
// version-control information the Go toolchain records (-buildvcs), which a
// build from a checkout carries and one installed from the module proxy does
// not, and then stays "unknown".
func Describe(name, release string) string {
	info, _ := debug.ReadBuildInfo()
	return describe(name, release, Commit, Date, info, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func describe(name, release, commit, date string, info *debug.BuildInfo, goVersion, goos, goarch string) string {
	if info != nil {
		settings := map[string]string{}
		for _, setting := range info.Settings {
			settings[setting.Key] = setting.Value
		}
		if commit == "unknown" && settings["vcs.revision"] != "" {
			commit = settings["vcs.revision"]
			if settings["vcs.modified"] == "true" {
				commit += "-dirty"
			}
		}
		if date == "unknown" && settings["vcs.time"] != "" {
			date = settings["vcs.time"]
		}
	}
	if len(commit) > shortCommit {
		commit = commit[:shortCommit] + commitSuffix(commit)
	}
	return fmt.Sprintf("%s %s (%s, %s, %s %s/%s)", name, release, commit, date, goVersion, goos, goarch)
}

// commitSuffix keeps the "-dirty" mark of a shortened revision.
func commitSuffix(commit string) string {
	const dirty = "-dirty"
	if len(commit) > len(dirty) && commit[len(commit)-len(dirty):] == dirty {
		return dirty
	}
	return ""
}
