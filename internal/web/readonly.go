package web

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"knomit/internal/auth"
	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// mcpRoutePattern matches the MCP dispatch endpoints and their subtrees, which
// are POST-for-reads and must bypass the read-only method gate. Read-only-ness
// for MCP is enforced by tool filtering in mcp.NewServer instead.
//
// Three route shapes reach the same dispatcher (see router.go): the
// branch-scoped /repos/{repo}/branches/{branch}/mcp route, the lens-scoped
// /lenses/{lens}/mcp route, and the session-bound /mcp mount. All three must
// bypass the method gate; the lens REST CRUD endpoints (/lenses and
// /lenses/{lens}) are deliberately NOT matched so they stay gated.
//
// The bare /mcp alternative matters most in read-only mode: initialize and
// knomit_bind are themselves POSTs, so gating them would make a read-only
// instance unreachable through the no-flag bridge — nothing could ever be
// bound, and therefore nothing read. Read-only-ness there is enforced where it
// is for the other two mounts: by tool filtering in mcp.NewServer.
//
// The gate runs on r.URL.Path, which retains the full APIBase prefix even
// though the handler is inside a chi sub-router mounted at APIBase. Each
// alternative is anchored (the mcp segment is followed by / or end-of-string)
// so that arbitrary …/facts/* paths that happen to contain a /branches/X/mcp or
// /lenses/X/mcp segment cannot bypass the gate.
var mcpRoutePattern = regexp.MustCompile("^" + regexp.QuoteMeta(APIBase) +
	`(?:/repos/[^/]+/branches/[^/]+/mcp|/lenses/[^/]+/mcp|/mcp)(/|$)`)

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

// isGitFetch reports whether a path is git's FETCH endpoint. upload-pack is
// how a client reads — clone and fetch both POST to it — so gating by HTTP
// method alone would refuse every clone.
//
// Anchored on the FINAL path segment, deliberately. splitGitRoute trims one
// ".git" suffix, so "/git/knomit.git/git-upload-pack" is a live route and a
// prefix or contains test would have to enumerate the spellings of the repo
// segment. Repo names admit only [a-z0-9-_] (isValidRepoName) and so contain
// no slash, which makes the last segment unambiguous.
func isGitFetch(path string) bool {
	return strings.HasSuffix(path, "/git-upload-pack")
}

// isGitPush reports whether a request is git's PUSH conversation: the
// receive-pack advertisement or the receive-pack POST. It is exempt from the
// write gate because a push is not a fact write: its permission is push:own,
// recorded by pushPermission and decided per request in GitRemoteHandler and
// the store's receive-pack (F11). Anchored on the final segment for the same
// reason as isGitFetch.
func isGitPush(r *http.Request) bool {
	if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
		return true
	}
	return strings.HasSuffix(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == "git-receive-pack"
}

// writeGate is the permission twin of readOnlyGate: a mutating request needs
// the write permission. ReadOnly (the demo) is the special case "nobody has
// write" and keeps its own message, because it is a property of the INSTANCE
// and not of the caller -- it runs first so its message wins.
//
// It reuses isMutatingRequest, which already exempts the MCP dispatch routes:
// those are POST-for-reads, and gating them by method would make an instance
// unreachable through the bridge, since initialize and knomit_bind are
// themselves POSTs. MCP write enforcement is per TOOL, in internal/mcp.
func writeGate(g auth.Grants, disabled bool) func(http.Handler) http.Handler {
	requireWrite := Require(g, auth.Write)
	return func(next http.Handler) http.Handler {
		if disabled {
			return next
		}
		gated := requireWrite(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isMutatingRequest(r.Method, r.URL.Path) && !isGitFetch(r.URL.Path) && !isGitPush(r) {
				gated.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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

type pushOwnKey struct{}

// pushPermission records, for a push request only, whether the principal
// holds push:own — the permission check for F11's receive-pack, run at the
// /git mount where the Grants are (the store handler has none). It refuses
// nothing itself: GitRemoteHandler turns the answer into the git-protocol
// refusal a client can read, which a 403 here would not be.
func pushPermission(g auth.Grants) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isGitPush(r) {
				p, _ := auth.FromContext(r.Context())
				ok := auth.Allowed(r.Context(), g, p, auth.PushOwn)
				r = r.WithContext(context.WithValue(r.Context(), pushOwnKey{}, ok))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// holdsPushOwn is pushPermission's answer; false when it did not run.
func holdsPushOwn(r *http.Request) bool {
	ok, _ := r.Context().Value(pushOwnKey{}).(bool)
	return ok
}

// requireOperator is Require(operator), switched off exactly when writeGate is
// (Form B, authDisabled), so a test server with auth disabled is not refused
// on one route alone.
func requireOperator(g auth.Grants, disabled bool) func(http.Handler) http.Handler {
	if disabled {
		return func(next http.Handler) http.Handler { return next }
	}
	return Require(g, auth.Operator)
}
