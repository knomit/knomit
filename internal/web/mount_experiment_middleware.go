package web

import (
	"context"
	"net/http"

	"github.com/rs/zerolog/log"

	"knomit/internal/client/sessions"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// MountExperimentMiddleware puts a URL-SCOPED MCP session inside the experiment
// it opened, with no reconnect and no bridge flag.
//
// WHY THIS IS SESSION-KEYED WHEN THE UNSCOPED MOUNT'S IS NOT. On the unscoped
// mount the caller names its target per call, so session-keyed routing there
// decides WHICH KNOWLEDGE BASE a write lands in — the 2026-09-17 incident, and
// the reason binding_handles exists. Here the URL has already fixed the repo or
// lens before this runs, so the only thing this state can change is the BRANCH
// within that one repo. Keyed on (session id, mount uid) so a client holding a
// repo bridge and a lens bridge at once keeps them apart. Migration 000008
// carries the full argument and what is knowingly accepted: two logical jobs
// sharing one connection share one row, so one opening an experiment moves the
// other with it.
//
// IT RUNS ONLY ON THE MCP MOUNTS. The REST routes in the same subtree name
// their branch in the URL and must keep meaning exactly that — a GET of
// /branches/agent:x/facts is a question about agent:x whoever asks it.
//
// LAZY HEAL, the same two conditions as the handle path
// (kb/invariants/mcp/experiments/lazy-heal-at-resolution): the experiment must
// still EXIST and still be ELIGIBLE. One removed by the sweeper, by a REST
// rollback, by another session committing it, or by its session row being
// purged falls back to the URL's own branch and the request is MARKED, so the
// result can say so. Failing instead would strand a session over someone
// else's action.
func MountExperimentMiddleware(m *repos.Manager, sessions *sessions.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := applyMountExperiment(r.Context(), m, sessions, r.Header.Get("Mcp-Session-Id"))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// applyMountExperiment is the middleware's body, factored out so tests can
// drive it without an HTTP request.
func applyMountExperiment(ctx context.Context, m *repos.Manager, store_ *sessions.Store, sessionID string) context.Context {
	if m == nil || store_ == nil {
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

	// WHICH MOUNTS TAKE SESSION STATE, decided from the ROUTE and never from
	// the binding.
	//
	// `b.WriteBranch()` is unusable for this: it returns "" whenever the mount
	// is not writable, so a URL naming `main` looks exactly like a URL naming
	// nothing — and a read-only mount would be silently re-pinned onto a
	// writable experiment, which is a bigger hole than the one this feature
	// was built to close.
	//
	//   repo route  (/repos/{repo}/branches/{branch}/mcp): re-pin ONLY when
	//     the URL names THIS repo's own agent branch. `main`, a foreign
	//     `agent/*` and an `exp:` branch are all addresses of something else
	//     and are left exactly as the URL asked — the last of those is what
	//     stops …/exp:a/mcp serving exp/b.
	//   lens route  (/lenses/{lens}/mcp): NO {branch} segment at all, so the
	//     URL branch is empty and the mount is the lens's own default. Re-pin.
	//     A predicate written only for the repo route would silently disable
	//     experiments for every `kb --lens` bridge.
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

	uid := mountExperimentUID(b)
	if uid == "" {
		return ctx
	}
	// Carried down to the tool handlers so `open` can RECORD against the same
	// (session, mount) pair this resolves against. Derived once, here, rather
	// than twice — a tool computing its own key is how a write and a read of
	// the same state start disagreeing.
	//
	// Set BEFORE the session-id check, because its presence is what tells the
	// tool this mount takes session state at all. Absent means the URL names
	// an experiment, which is a different refusal.
	ctx = repos.WithMountSession(ctx, sessionID, uid)
	if sessionID == "" {
		// FAIL CLOSED: keying on ("", mount uid) would pool every anonymous
		// caller of this mount into one shared experiment.
		return ctx
	}

	name, err := store_.MountExperiment(ctx, sessionID, uid)
	if err != nil {
		// A read failure is NOT "no experiment": "" is a routing ANSWER, and
		// taking it from a failed query would silently move the session back
		// onto the agent branch mid-work.
		log.Warn().Err(err).Str("mount", uid).Msg("mount experiment lookup failed; staying on the branch the URL names")
		return ctx
	}
	if name == "" {
		return ctx
	}

	expBranch := store.ExperimentBranch(name)
	write := b.Write()
	if write == nil || !write.WritableBranch(expBranch) {
		// Gone, or no longer ours. Fall back and SAY SO — being moved without
		// being told is how the next write lands on the wrong branch.
		return repos.WithLapsedExperiment(ctx, name)
	}

	if b.FromLens() {
		// Re-resolve the lens from its uid and re-pin ONLY its write member,
		// exactly as the handle path does: a lens experiment forks the write
		// repo and leaves every read mount where it was
		// (kb/decisions/experiments/lens).
		_, uidOnly, perr := repos.ParsePin(b.PinID())
		if perr != nil {
			return repos.WithLapsedExperiment(ctx, name)
		}
		reg := m.LensRegistry()
		if reg == nil {
			return repos.WithLapsedExperiment(ctx, name)
		}
		l, lok, lerr := reg.GetByUID(uidOnly)
		if lerr != nil || !lok {
			return repos.WithLapsedExperiment(ctx, name)
		}
		nb, berr := repos.NewBindingOfLensOnExperiment(m, l, name)
		if berr != nil {
			return repos.WithLapsedExperiment(ctx, name)
		}
		ctx = repos.WithBinding(ctx, nb)
		return repos.WithRepoInstance(ctx, nb.Write())
	}

	nb := repos.NewBindingOfRepo(write, expBranch)
	ctx = repos.WithBinding(ctx, nb)
	return repos.WithRepoInstance(ctx, nb.Write())
}

// mountExperimentUID is the mount's identity for the storage key: the lens uid
// on a lens mount, the write repo's uid otherwise.
//
// It is the UID and never the NAME, because a rename must not silently hand a
// session's experiment to a different mount, nor orphan it from its own.
func mountExperimentUID(b *repos.Binding) string {
	if b == nil {
		return ""
	}
	// PinID is already `repo:<uid>` or `lens:<uid>` — the mount's identity in
	// the one spelling the rest of the binding layer uses. Deriving a second
	// spelling here is how two keys for one mount start disagreeing.
	return b.PinID()
}
