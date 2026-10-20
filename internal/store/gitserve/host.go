package gitserve

import (
	"net/http"

	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/remotesync"
)

// host is what the smart-HTTP handler reads a store through: the one shared
// vcs handle (refs, objects, branch locks, the commit chokepoint) and the remotesync
// handle for the one move receive-pack makes through reconcile code
// (AdvanceBranchTo). It holds no state of its own.
type host struct {
	*vcs.Repo
	sy *remotesync.Repo
}

// NewHandler returns the Smart HTTP git handler for the store behind r and
// sy. upstream is evaluated per request (see newGitHTTPHandler). The parent
// builds it once per Service and caches it; everything per-request, above all
// who is pushing, arrives on the request context (WithPushPolicy).
func NewHandler(r *vcs.Repo, sy *remotesync.Repo, upstream func() string) http.Handler {
	return newGitHTTPHandler(&host{Repo: r, sy: sy}, upstream)
}
