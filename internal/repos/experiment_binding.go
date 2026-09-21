// Experiment-aware binding resolution: turning "this handle is working inside
// exp/<name>" into a Binding that reads and writes there.
package repos

import (
	"context"
	"fmt"

	"knomit/internal/store"
)

// lapsedExperimentKey is the private context key for an experiment that was
// asked for and could not be honoured.
type lapsedExperimentKey struct{}

// WithLapsedExperiment records that the caller's stored experiment could not
// be honoured and the ordinary binding was used instead.
//
// It is a context value rather than an error because the call MUST still run:
// an experiment can vanish under a live caller through the expiry sweeper, a
// rollback from the REST API or the UI, or another session committing a
// shared experiment. None of those are the caller's mistake, and failing its
// next query over one would be a worse answer than serving the agent branch
// and saying so.
func WithLapsedExperiment(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, lapsedExperimentKey{}, name)
}

// LapsedExperimentFromContext returns the experiment that could not be
// honoured on this request, or "" when there was none.
func LapsedExperimentFromContext(ctx context.Context) string {
	name, _ := ctx.Value(lapsedExperimentKey{}).(string)
	return name
}

// ActiveExperiment returns the experiment a binding is writing inside, or ""
// when it is on an ordinary branch.
//
// DERIVED from the binding's own write branch rather than carried alongside
// it, so it answers identically on all three mounts: a handle that set one,
// a /repos/{repo}/branches/exp:<name>/mcp URL, and a
// /lenses/{lens}/experiments/{name}/mcp URL all produce a binding whose write
// branch is the experiment, and none of them needs a second source of truth
// that could disagree with where the write actually goes.
func ActiveExperiment(b *Binding) string {
	if b == nil || !b.WriteOK() {
		return ""
	}
	name, _ := store.ExperimentNameOf(b.WriteBranch())
	return name
}

// ResolveSessionBindingOnExperiment is ResolveSessionBinding for a caller that
// is working inside an experiment.
//
// An empty experiment is the ordinary path, unchanged. A non-empty one is
// applied ONLY when the write repo may actually write it — which
// WritableBranch answers from the experiments record, so a name whose
// experiment was committed, rolled back or swept, or whose parent is no
// longer this instance's agent branch, simply does not apply. The caller is
// then bound exactly as it would have been without an experiment, and
// WithLapsedExperiment marks the request so the tool layer can say what
// happened.
//
// That lazy heal is the load-bearing half of the lifecycle, not a courtesy.
// Eagerly clearing the handle at commit and rollback covers only the caller
// that issued them; the sweeper, the REST API and a second session sharing
// the experiment all remove one without any handle in sight. Resolution is
// the single place every later call passes through, so it is the only place
// that can be right for all of them.
func ResolveSessionBindingOnExperiment(ctx context.Context, m *Manager, pin, branch, experiment string) (context.Context, error) {
	if experiment == "" {
		return ResolveSessionBinding(ctx, m, pin, branch)
	}
	// The read-side half of the same rule the store enforces on write: a
	// handle carrying BOTH a branch pin and an experiment is corrupt, not a
	// precedence question. Unreachable — SetHandleExperiment refuses to
	// create the pair — and that is exactly why it fails loudly rather than
	// quietly preferring one, the same treatment the lens arm of
	// ResolveSessionBinding gives its own unreachable case.
	if branch != "" {
		return nil, &SessionBindingError{
			Kind: BindingMalformed, Pin: pin,
			Err: fmt.Errorf("binding names both branch %q and experiment %q, which cannot both be the write target — call knomit_bind again",
				branch, experiment),
		}
	}
	kind, uid, err := ParsePin(pin)
	if err != nil {
		// Malformed pin: let the ordinary path produce its classified error
		// rather than inventing a second wording for the same failure.
		return ResolveSessionBinding(ctx, m, pin, branch)
	}
	expBranch := store.ExperimentBranch(experiment)

	switch kind {
	case "repo":
		ri := m.GetByUID(uid)
		if ri == nil {
			return ResolveSessionBinding(ctx, m, pin, branch)
		}
		if !ri.WritableBranch(expBranch) {
			return ResolveSessionBinding(WithLapsedExperiment(ctx, experiment), m, pin, branch)
		}
		// The experiment REPLACES the handle's read branch. A handle cannot
		// hold both: the guard above refuses the combination outright.
		b := NewBindingOfRepo(ri, expBranch)
		ctx = WithBinding(ctx, b)
		ctx = WithRepoInstance(ctx, b.Write())
		return ctx, nil

	case "lens":
		reg := m.LensRegistry()
		if reg == nil {
			return ResolveSessionBinding(ctx, m, pin, branch)
		}
		l, ok, lerr := reg.GetByUID(uid)
		if lerr != nil || !ok {
			return ResolveSessionBinding(ctx, m, pin, branch)
		}
		write := m.GetByUID(l.WriteUID)
		if write == nil {
			return ResolveSessionBinding(ctx, m, pin, branch)
		}
		if !write.WritableBranch(expBranch) {
			return ResolveSessionBinding(WithLapsedExperiment(ctx, experiment), m, pin, branch)
		}
		b, berr := NewBindingOfLensOnExperiment(m, l, experiment)
		if berr != nil {
			return ResolveSessionBinding(ctx, m, pin, branch)
		}
		ctx = WithBinding(ctx, b)
		ctx = WithRepoInstance(ctx, b.Write())
		return ctx, nil
	}
	return ResolveSessionBinding(ctx, m, pin, branch)
}

// mountSessionKey carries the MCP session id and mount identity from the HTTP
// middleware down to the tool handlers.
type mountSessionKey struct{}

type mountSession struct {
	SessionID string
	MountUID  string
}

// WithMountSession marks this request as arriving on a URL-scoped mount whose
// URL names a PLAIN branch — one where session state may decide the branch —
// and carries the session id when there is one.
//
// It lives here rather than in internal/web because internal/mcp must READ it
// and cannot import internal/web.
//
// THE MARKER IS SET EVEN WITH AN EMPTY SESSION ID, and that distinction is
// load-bearing: "this mount accepts session state but you sent no session id"
// and "this mount addresses an experiment by URL" are different refusals with
// different fixes, and a caller told the wrong one goes looking in the wrong
// place. Presence answers the first question, SessionID the second.
func WithMountSession(ctx context.Context, sessionID, mountUID string) context.Context {
	if mountUID == "" {
		return ctx
	}
	return context.WithValue(ctx, mountSessionKey{}, mountSession{SessionID: sessionID, MountUID: mountUID})
}

// MountSessionFromContext returns the session id and mount identity for a
// URL-scoped MCP request on a PLAIN-branch mount.
//
// ok is false on the unscoped mount, on a mount whose URL names an experiment,
// and on every REST route — none of which may record or read a mount
// experiment. ok true with an empty sessionID means the mount would accept
// session state but the client sent no session id.
func MountSessionFromContext(ctx context.Context) (sessionID, mountUID string, ok bool) {
	ms, found := ctx.Value(mountSessionKey{}).(mountSession)
	if !found {
		return "", "", false
	}
	return ms.SessionID, ms.MountUID, true
}
