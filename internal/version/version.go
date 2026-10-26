package version

// Build information, set at link time (-ldflags -X) and "dev" or "unknown" in
// a plain go build.
var (
	// Version is the release version of the forge command.
	Version = "dev"
	// Commit is the source revision the command was built from.
	Commit = "unknown"
	// Date is when the command was built.
	Date = "unknown"
)
