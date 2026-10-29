package web

import (
	"context"
	"net/http"

	"knomit/internal/corpus/repos"
)

// ScopedMountMiddleware marks a URL-SCOPED MCP request whose URL names the
// repo's own agent branch, or a lens's default, as a mount where a binding
// HANDLE may choose the branch (knomit_bind, `binding`, knomit_experiment).
//
// THE DEFAULT NEVER MOVES (#685). This middleware used to also read a
// session-keyed (Mcp-Session-Id, mount uid) row and swap the request's binding
// onto the experiment the SESSION had opened. That is gone, and nothing
// replaces it: a call without a handle is always served the branch the URL
// names. Subagents share their parent's MCP session and nothing on the wire
// tells them apart, so a session-keyed default moved every agent on the
// session — reads went stale and writes landed in someone else's experiment.
// An experiment is reached ONLY through a handle (`knomit_experiment open`
// returns one). Rows a server from before this change left in
// mount_experiments are never read, and the client-session purge drops them.
//
// IT RUNS ONLY ON THE MCP MOUNTS. The REST routes in the same subtree name
// their branch in the URL and must keep meaning exactly that.
func ScopedMountMiddleware(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := markScopedMount(r.Context(), m, r.Header.Get("Mcp-Session-Id"))
			// The same return path the unscoped mount installs: a call routed
			// by a binding HANDLE here is resolved inside the tool gate, and
			// the recorder carries that handle back out to
			// recordClientSession (the session's binding set) and to the
			// request log's served branch. A call without a handle records
			// nothing, and both fall back to the URL's binding.
			ctx = repos.WithPinRecorder(ctx, &repos.PinRecorder{})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// markScopedMount is the middleware's body, factored out so tests can drive it
// without an HTTP request. It never changes the request's binding: it only
// adds the mount marker that lets a handle choose a branch.
func markScopedMount(ctx context.Context, m *repos.Manager, sessionID string) context.Context {
	if m == nil {
		return ctx
	}
	b, ok := repos.BindingFromContextOpt(ctx)
	if !ok {
		ri, riOK := repos.RepoFromContextOpt(ctx)
		if !riOK {
			return ctx
		}
		branch, _ := repos.BranchFromContextOpt(ctx)
		b = repos.NewBindingOfRepo(ri, branch)
	}

	// WHICH MOUNTS TAKE A HANDLE, decided from the ROUTE and never from the
	// binding (kb/invariants/mcp/experiments/mount-eligibility-from-the-route).
	//
	// `b.WriteBranch()` is unusable for this: it returns "" whenever the mount
	// is not writable, so a URL naming `main` looks exactly like a URL naming
	// nothing.
	//
	//   repo route  (/repos/{repo}/branches/{branch}/mcp): ONLY when the URL
	//     names THIS repo's own agent branch. `main`, a foreign `agent/*` and
	//     an `exp:` branch are addresses of something else and take no handle.
	//   lens route  (/lenses/{lens}/mcp): NO {branch} segment at all, so the
	//     mount is the lens's own default.
	//
	// /lenses/{lens}/experiments/{name}/mcp never installs this middleware, so
	// it needs no case here and must keep not having one.
	urlBranch, hasURLBranch := repos.BranchFromContextOpt(ctx)
	if hasURLBranch {
		write := b.Write()
		if write == nil || urlBranch == "" || urlBranch != write.AgentBranch() {
			return ctx
		}
	} else if !b.FromLens() {
		// Neither a branch route nor a lens: not a mount this applies to.
		return ctx
	}

	// PinID is `repo:<uid>` or `lens:<uid>`: the mount's identity, by uid so a
	// rename cannot hand a handle to another mount. Handles minted here are
	// recorded against it and work nowhere else.
	uid := b.PinID()
	if uid == "" {
		return ctx
	}
	return repos.WithMountSession(ctx, sessionID, uid)
}

// servedBranch is the branch this request was actually served on, logged
// whenever the URL does not ALREADY NAME that branch — and "" when it does.
//
// It exists for the log line: a session inside an experiment is served
// exp/<name> while its path still reads …/branches/agent:…/mcp, so a log
// carrying only the path answers "where did that fact go" wrongly and with no
// sign of it.
//
// Note the condition is "the URL does not already say it", not "the two
// differ". A lens mount carries NO {branch} segment, so its URL never names a
// branch and the field is present on every one of its lines — which is right:
// there the path alone has never said where a write goes. Only a repo mount,
// whose path does name a branch, can be in the case where the field would
// merely repeat it, and there it is omitted.
func servedBranch(ctx context.Context) string {
	// A call routed by a binding HANDLE was served where the handle points,
	// which the HTTP context cannot know: the gate resolved it inside the
	// handler and recorded it here.
	if p, ok := repos.PinRecorderFromContext(ctx); ok {
		if rb := p.Resolved(); rb.Handle != "" && rb.Served != "" {
			if urlBranch, hasURL := repos.BranchFromContextOpt(ctx); hasURL && urlBranch == rb.Served {
				return ""
			}
			return rb.Served
		}
	}
	b, ok := repos.BindingFromContextOpt(ctx)
	if !ok {
		return ""
	}
	served := b.WriteBranch()
	if served == "" {
		return ""
	}
	if urlBranch, hasURL := repos.BranchFromContextOpt(ctx); hasURL && urlBranch == served {
		return ""
	}
	return served
}
