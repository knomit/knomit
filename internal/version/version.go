// Package version holds the build version of every knomit binary.
//
// Version, Commit and UpdatePublicKey are injected at build time via the Go
// linker:
//
//	go build -ldflags "-X knomit/internal/version.Version=0.5.0 \
//	                   -X knomit/internal/version.Commit=$(git rev-parse --short HEAD) \
//	                   -X knomit/internal/version.UpdatePublicKey=$UPDATE_PUBLIC_KEY"
//
// The Makefile is the single source of truth for all three values. The defaults
// below keep a bare `go build` (and tests, and the IDE) compiling and
// reporting a "dev" version rather than an empty string.
package version

var (
	// Version is the Major.Minor.Patch semver, set from the Makefile.
	Version = "dev"
	// Commit is the short git SHA of the build, set from the Makefile.
	Commit = ""
	// UpdatePublicKey is the base64-encoded Ed25519 public key that
	// authenticates desktop update artifacts, injected by the stable-release
	// workflow.
	//
	// Empty means self-update is disabled ENTIRELY — not "install updates
	// without checking signatures". pkg/updater's verification fails open on a
	// release that carries no verification block, and its Config offers no way
	// to demand a signature, so the presence of this key is what gates the
	// whole feature. See tools/desktop/update.go.
	UpdatePublicKey = ""
)

// String returns the full build version "<semver>.<short-sha>", or just the
// semver when no commit was injected (a bare `go build`).
func String() string {
	if Commit == "" {
		return Version
	}
	return Version + "." + Commit
}
