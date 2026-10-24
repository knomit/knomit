package httpx

import (
	"fmt"
	"net/http"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
)

// RefuseUnwritableBranch answers 403 when facts may not be authored on branch
// through ri — the REST twin of the MCP write tools' WriteOK check. It
// consults the same classification (RepoInstance.WritableBranch), so a
// subscription, the consensus branch, and a foreign agent branch are all
// refused here with a named reason rather than by the store's read-only flag.
//
// The store's flag is the structural guarantee and stays the backstop; this is
// what turns "the store refused" into an answer a client can act on.
func RefuseUnwritableBranch(w http.ResponseWriter, r *http.Request, ri *repos.RepoInstance, branch string) bool {
	if ri.WritableBranch(branch) {
		return false
	}
	// The detail has to differ, because the advice does. Pointing a
	// subscription's client at "the repo's own agent branch" names a branch
	// that does not exist — and which repoView deliberately omits from the DTO
	// for exactly that reason.
	detail := fmt.Sprintf("facts cannot be written to branch %q of repo %q; only the repo's own agent branch accepts writes", branch, ri.Name())
	switch {
	case ri.Subscribed():
		detail = fmt.Sprintf("repo %q is a subscription: it follows a remote branch read-only and accepts no fact writes on any branch, including %q", ri.Name(), branch)
	}
	hal.WriteProblem(w, http.StatusForbidden, "Read-only branch", detail, r.URL.Path)
	return true
}
