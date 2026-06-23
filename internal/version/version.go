// Package version holds the build version of every knomit binary.
//
// Version and Commit are injected at build time via the Go linker:
//
//	go build -ldflags "-X knomit/internal/version.Version=0.5.0 \
//	                   -X knomit/internal/version.Commit=$(git rev-parse --short HEAD)"
//
// The Makefile is the single source of truth for both values. The defaults
// below keep a bare `go build` (and tests, and the IDE) compiling and
// reporting a "dev" version rather than an empty string.
package version

var (
	// Version is the Major.Minor.Patch semver, set from the Makefile.
	Version = "dev"
	// Commit is the short git SHA of the build, set from the Makefile.
	Commit = ""
)

// String returns the full build version "<semver>.<short-sha>", or just the
// semver when no commit was injected (a bare `go build`).
func String() string {
	if Commit == "" {
		return Version
	}
	return Version + "." + Commit
}
