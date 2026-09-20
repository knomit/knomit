package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"knomit/internal/client/sessions"
	"knomit/internal/repos"
)

// bindTool defines knomit_bind: the caller's choice of repo or lens.
//
// The description is what an agent reads before its first call on a fresh
// session, so it states the things it cannot discover by trying: that the
// result carries a HANDLE every other tool needs, that a subscription serves
// reads but refuses writes, and that the result carries the bound base's
// instructions.
//
// It declares no `binding` argument of its own — knomit_bind is where handles
// come from — so rejectUnknownArguments refuses one, which is the right answer
// for an agent reflexively attaching its handle to every call.
func bindTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_bind",
		mcpgo.WithDescription("Only on the unscoped /api/v1/mcp endpoint (a bridge started with neither --repo nor --lens); on a URL-scoped endpoint this always fails. Bind a repo or lens by name (see knomit_repos for the names) and receive an opaque `binding` handle. EVERY other tool requires that handle as its `binding` argument — keep it for the rest of your work and pass it on every call. Required before any other tool works; call again for a different base, which mints a SECOND handle and leaves the first one valid. Writes go to the repo's agent branch; a subscribed (read-only follower) repo binds at the branch it follows and refuses writes. Returns the handle, the mounts table and the knowledge base's instructions — treat them as session instructions."),
		mcpgo.WithString("repo", mcpgo.Description("Name of the repo to bind to. Mutually exclusive with lens.")),
		mcpgo.WithString("lens", mcpgo.Description("Name of the lens to bind to. Mutually exclusive with repo.")),
		mcpgo.WithString("experiment", mcpgo.Description("Optional: RESUME an existing experiment, so the new handle starts inside it. Does not create one — use knomit_experiment {action: \"open\"} for that. This is how you get back into an experiment after reconnecting, since a handle does not outlive its session.")),
	)
}

// BindHandler returns the handler for knomit_bind.
//
// It binds by NAME because that is what the agent knows, but stores the uid
// pin: a rename must not strand a live handle. The binding is then built
// in-process rather than re-read, so the result describes exactly what was
// persisted.
//
// EVERY CALL MINTS A FRESH HANDLE, and old handles stay valid. That is what
// makes two concurrent callers on one connection safe: each holds a handle that
// names what it named when minted, and neither can overwrite the other. An
// agent that binds twice by mistake ends up holding two working handles rather
// than one broken one — a strictly better failure than the upsert this
// replaced, where the second bind silently redirected the first caller's
// writes.
//
// There is deliberately no unbind form: a handle is abandoned, not cleared. It
// ages out of binding_handles once unused for the retention window.
func BindHandler(mgr *repos.Manager) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if err := rejectUnknownArguments(req, bindTool()); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		// A URL-scoped mount has nothing to switch: its repo comes from the
		// path, and rebinding it would silently serve a different base than the
		// endpoint names.
		if !repos.SessionScoped(ctx) {
			return mcpgo.NewToolResultError(
				"this endpoint is bound by its URL; connect the bridge to /api/v1/mcp (no --repo/--lens) to switch repos"), nil
		}

		repoName := req.GetString("repo", "")
		lensName := req.GetString("lens", "")
		experiment := req.GetString("experiment", "")
		switch {
		case repoName == "" && lensName == "":
			return mcpgo.NewToolResultError(
				"knomit_bind needs exactly one of `repo` or `lens` (a name); there is no unbind form"), nil
		case repoName != "" && lensName != "":
			return mcpgo.NewToolResultError(
				"knomit_bind takes exactly one of `repo` or `lens`, not both"), nil
		}

		if mgr == nil {
			return mcpgo.NewToolResultError(errStoreUnavailable.Error()), nil
		}
		store := mgr.ClientSessions()
		if store == nil {
			return mcpgo.NewToolResultError(
				"session store unavailable — this server cannot mint a binding handle right now"), nil
		}
		// No MCP session id is read, and none is needed. The handle is the key,
		// which is exactly why two callers sharing one session id no longer
		// collide.

		b, pin, err := bindTarget(mgr, repoName, lensName)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		// RESUME ONLY. Binding is not where experiments are created: an
		// agent that mistypes a name here should be told so, not handed a
		// fresh branch it did not ask for and will not find its work on.
		// Checked BEFORE the handle is minted, so a refusal leaves nothing
		// behind.
		if experiment != "" {
			resumed, rerr := resumeExperimentBinding(ctx, mgr, b, experiment)
			if rerr != nil {
				return mcpgo.NewToolResultError(rerr.Error()), nil
			}
			b = resumed
		}
		handle, err := sessions.NewBindingHandle()
		if err != nil {
			log.Error().Err(err).Msg("knomit_bind: minting a handle failed")
			return mcpgo.NewToolResultError(
				"a binding handle could not be generated, so nothing was bound; ask the operator to check the server log"), nil
		}
		// No branch: knomit_bind does not take one yet, and "" already means
		// the target's own read branch. The column is carried so the planned
		// per-handle branch switch is a behaviour change, not a migration.
		if err := store.MintBindingHandle(ctx, handle, pin, "", time.Now()); err != nil {
			// Worth a log line: the likely cause is a control.db that never got
			// migration 000005, and without server-side evidence that presents
			// to the agent as an unexplained refusal it would retry forever.
			log.Warn().Err(err).Str("pin", pin).
				Msg("knomit_bind: recording the binding handle failed")
			return mcpgo.NewToolResultError(
				"the binding could not be recorded, so nothing was bound; ask the operator to check the server log"), nil
		}

		// The experiment goes on the handle only after the handle exists, and
		// a failure here is reported rather than swallowed: a caller told it
		// is inside an experiment that is not would write its next fact to
		// the agent branch.
		if experiment != "" {
			if serr := setHandleExperiment(ctx, mgr, handle, experiment); serr != nil {
				return mcpgo.NewToolResultError(serr.Error()), nil
			}
		}

		out, err := json.MarshalIndent(bindResultOf(handle, b), "", "  ")
		if err != nil {
			return mcpgo.NewToolResultError("marshal error: " + err.Error()), nil
		}
		// initialize on the unscoped mount could only carry the DEFAULT
		// ontology, and MCP cannot re-send instructions mid-session, so the
		// bound base's own instructions have to ride back here — behind the
		// handle banner, which has to be the first thing read.
		return mcpgo.NewToolResultText(
			string(out) + "\n\n" + handleBanner(handle) +
				BindingInstructions(b, profileFor(mgr, b.Write()))), nil
	}
}

// bindTarget resolves the named repo or lens to a binding plus the pin to
// store. Exactly one of repoName/lensName is non-empty.
func bindTarget(mgr *repos.Manager, repoName, lensName string) (*repos.Binding, string, error) {
	if repoName != "" {
		ri := mgr.Get(repoName)
		if ri == nil {
			// Registered but storeless is a different answer from absent, and
			// only one of them leaves the caller something to do — the same
			// split RepoMiddleware renders as 409 versus 404.
			for _, u := range mgr.Unavailable() {
				if u.Record.Name == repoName {
					return nil, "", fmt.Errorf("repo %q is registered but has no store (%s): %s",
						repoName, u.Reason, u.Detail)
				}
			}
			return nil, "", fmt.Errorf("no repo named %q", repoName)
		}
		// "" ⇒ the repo's own read branch: the agent branch, or the followed
		// upstream for a subscription, with writeOK following WritableBranch.
		return repos.NewBindingOfRepo(ri, ""), repos.PinForRepo(ri), nil
	}

	reg := mgr.LensRegistry()
	if reg == nil {
		return nil, "", fmt.Errorf("lens registry not started")
	}
	l, ok, err := reg.Get(lensName)
	if err != nil {
		return nil, "", fmt.Errorf("lens registry is unavailable — retry shortly")
	}
	if !ok {
		return nil, "", fmt.Errorf("no lens named %q", lensName)
	}
	b, err := repos.NewBindingOfLens(mgr, l)
	if err != nil {
		return nil, "", err
	}
	return b, repos.PinForLens(l), nil
}

// resumeExperimentBinding re-binds b onto an EXISTING experiment, or explains
// why it cannot.
//
// The three refusals are distinct because the repairs are: a name nobody
// opened wants knomit_experiment open, an experiment forked from a branch this
// instance no longer writes wants a rollback, and a subscription wants neither
// because it can never hold one.
func resumeExperimentBinding(ctx context.Context, mgr *repos.Manager, b *repos.Binding, experiment string) (*repos.Binding, error) {
	ri := b.Write()
	if ri.Subscribed() {
		return nil, fmt.Errorf("repo %q is a subscription and can hold no experiment", ri.Name())
	}
	svc, release, err := ri.Acquire()
	if err != nil {
		return nil, errStoreUnavailable
	}
	defer release()

	exp, ok, err := svc.Experiments().GetExperiment(ctx, experiment)
	if err != nil {
		return nil, fmt.Errorf("could not read experiment %q: %w", experiment, err)
	}
	if !ok {
		return nil, fmt.Errorf(
			"no experiment named %q on repo %q — knomit_bind RESUMES an experiment, it does not create one; "+
				"call knomit_experiment {action: \"open\", name: %q} to start it, or {action: \"list\"} to see what exists",
			experiment, ri.Name(), experiment)
	}
	if !ri.WritableBranch(exp.Branch()) {
		return nil, fmt.Errorf(
			"experiment %q was forked from %q, which is not this instance's agent branch %q — "+
				"roll it back, or work on it from the instance that owns that branch",
			experiment, exp.Parent, ri.AgentBranch())
	}

	if b.FromLens() {
		reg := mgr.LensRegistry()
		if reg == nil {
			return nil, fmt.Errorf("lens registry not started")
		}
		l, found, lerr := reg.Get(b.Name())
		if lerr != nil || !found {
			return nil, fmt.Errorf("lens %q is no longer available", b.Name())
		}
		return repos.NewBindingOfLensOnExperiment(mgr, l, experiment)
	}
	return repos.NewBindingOfRepo(ri, exp.Branch()), nil
}
