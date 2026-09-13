package web

import (
	"fmt"
	"net/http"
	"regexp"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// mcpRoutePattern matches the MCP dispatch endpoints and their subtrees, which
// are POST-for-reads and must bypass the read-only method gate. Read-only-ness
// for MCP is enforced by tool filtering in mcp.NewServer instead.
//
// Two route shapes reach the same dispatcher (see router.go): the branch-scoped
// /repos/{repo}/branches/{branch}/mcp route and the lens-scoped /lenses/{lens}/mcp
// route. Both must bypass the method gate; the lens REST CRUD endpoints
// (/lenses and /lenses/{lens}) are deliberately NOT matched so they stay gated.
//
// The gate runs on r.URL.Path, which retains the full APIBase prefix even
// though the handler is inside a chi sub-router mounted at APIBase. Each
// alternative is anchored (the mcp segment is followed by / or end-of-string)
// so that arbitrary …/facts/* paths that happen to contain a /branches/X/mcp or
// /lenses/X/mcp segment cannot bypass the gate.
var mcpRoutePattern = regexp.MustCompile("^" + regexp.QuoteMeta(APIBase) +
	`(?:/repos/[^/]+/branches/[^/]+/mcp|/lenses/[^/]+/mcp)(/|$)`)

// isMutatingRequest reports whether a request would mutate state and therefore
// must be rejected in read-only mode. Mutating HTTP methods are gated unless
// the path is the MCP dispatch route.
func isMutatingRequest(method, path string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return !mcpRoutePattern.MatchString(path)
	default:
		return false
	}
}

// readOnlyGate rejects mutating requests with 403 problem+json. Mounted only
// when the server is in read-only mode.
func readOnlyGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMutatingRequest(r.Method, r.URL.Path) {
			hal.WriteProblem(w, http.StatusForbidden, "Read-only instance",
				"this knomit instance is running in read-only (demo) mode; mutations are disabled",
				r.URL.Path)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refuseUnwritableBranch answers 403 when facts may not be authored on branch
// through ri — the REST twin of the MCP write tools' WriteOK check. It
// consults the same classification (RepoInstance.WritableBranch), so a
// subscription, the consensus branch, and a foreign agent branch are all
// refused here with a named reason rather than by the store's read-only flag.
//
// The store's flag is the structural guarantee and stays the backstop; this is
// what turns "the store refused" into an answer a client can act on.
func refuseUnwritableBranch(w http.ResponseWriter, r *http.Request, ri *repos.RepoInstance, branch string) bool {
	if ri.WritableBranch(branch) {
		return false
	}
	// The detail has to differ, because the advice does. Pointing a
	// subscription's client at "the repo's own agent branch" names a branch
	// that does not exist — and which repoView deliberately omits from the DTO
	// for exactly that reason.
	detail := fmt.Sprintf("facts cannot be written to branch %q of repo %q; only the repo's own agent branch accepts writes", branch, ri.Name())
	if ri.Subscribed() {
		detail = fmt.Sprintf("repo %q is a subscription: it follows a remote branch read-only and accepts no fact writes on any branch, including %q", ri.Name(), branch)
	}
	hal.WriteProblem(w, http.StatusForbidden, "Read-only branch", detail, r.URL.Path)
	return true
}
