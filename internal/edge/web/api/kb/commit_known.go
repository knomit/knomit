package kb

import (
	"net/http"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/store"
)

// refuseUnknownCommit answers a commit-anchored route whose {sha} this repo
// does not hold, and reports whether the handler may go on. Such a commit is
// a 404 with code commit_not_in_repo, never a 500 (user ruling 2026-11-03):
// the lens UI keeps one mount's asOf commit when its re-select lands on
// another mount's fact, and that read must be recognisable, not an internal
// error. A store that cannot be reached is the usual 503.
//
// It runs AFTER the route's query-parameter refusal (#718/#720), so a
// malformed request is still 400 first, and BEFORE every read at the commit
// — fact, system file, edges, materialize — so none of them can turn the
// missing commit into its own "not found" or a 500.
func refuseUnknownCommit(w http.ResponseWriter, r *http.Request, ri *repos.RepoInstance, sha string) bool {
	var known bool
	if err := ri.WithRead(func(svc *store.Service) {
		known = svc.HasCommit(sha)
	}); err != nil {
		httpx.WriteStoreError(w, r, err, "Failed to resolve commit", httpx.BranchFromContext(r.Context()))
		return false
	}
	if !known {
		httpx.WriteCommitNotInRepo(w, r, sha)
		return false
	}
	return true
}
