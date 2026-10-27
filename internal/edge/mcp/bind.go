package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos"
	"knomit/internal/foundation/clientsessions"
)

// bindExperimentDescription is knomit_bind's `experiment` argument. One text
// for both endpoint kinds: resuming an experiment means the same thing on each.
const bindExperimentDescription = "Optional: RESUME an existing experiment, so the new handle starts inside it. Does not create one — use knomit_experiment {action: \"open\"} for that. This is how you get back into an experiment after reconnecting, since a handle does not outlive its session."

// bindTool defines knomit_bind as the UNSCOPED endpoint lists it: the caller's
// choice of repo or lens.
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
//
// This is also the schema the handler validates against on BOTH endpoint
// kinds. A URL-scoped endpoint lists scopedBindTool instead, which drops
// `repo` and `lens`; a caller there that sends one anyway gets the
// "this connection is fixed to …" answer rather than "unknown argument".
func bindTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_bind",
		mcpgo.WithDescription("Bind a repo or lens by name (see knomit_repos for the names) and receive an opaque `binding` handle. EVERY other tool except knomit_repos requires that handle as its `binding` argument — keep it for the rest of your work and pass it on every call. The handle selects the knowledge base and carries no review state: knomit_review and knomit_hypothesize continuations also need their own session_id. Nothing but knomit_repos works until you have one; call again for a different base, which mints a SECOND handle and leaves the first one valid. Writes go to the repo's agent branch; a subscribed (read-only follower) repo binds at the branch it follows and refuses writes. Returns the handle, the mounts table and the knowledge base's instructions — treat them as session instructions."),
		mcpgo.WithString("repo", mcpgo.Description("Name of the repo to bind to. Mutually exclusive with lens.")),
		mcpgo.WithString("lens", mcpgo.Description("Name of the lens to bind to. Mutually exclusive with repo.")),
		mcpgo.WithString("experiment", mcpgo.Description(bindExperimentDescription)),
	)
}

// scopedBindTool is knomit_bind as a URL-scoped endpoint lists it. The URL
// fixes the repo or lens, so the only thing left to choose is the branch: the
// default one, or an existing experiment. That is exactly what the unscoped
// bind can choose for a given repo or lens, which is the rule — a scoped
// endpoint binds whatever the unscoped one could, confined to its own target.
func scopedBindTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_bind",
		mcpgo.WithDescription("This connection is fixed to one repo or lens by its URL; knomit_bind here only changes the BRANCH you work on, never the repo or lens. Call it with no arguments for a new `binding` handle on the default branch, or with `experiment` to resume an existing experiment. Pass that handle as `binding` on later calls to work on its branch; calls without `binding` keep using the connection's own repo or lens. Each call mints a new handle and earlier ones stay valid, so several agents sharing this connection can each hold their own: bind, then knomit_experiment {action: \"open\", binding: <handle>} moves only that handle into its own experiment."),
		mcpgo.WithString("experiment", mcpgo.Description(bindExperimentDescription)),
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
// ON A URL-SCOPED ENDPOINT the target is the URL's repo or lens, never a name
// from the arguments, and the handle is recorded against that mount so it works
// there and nowhere else. Everything after choosing the target — experiment
// resume, minting, the result — is the same code as on the unscoped endpoint.
//
// There is deliberately no unbind form: a handle is abandoned, not cleared. It
// ages out of binding_handles once unused for the retention window.
func BindHandler(mgr *repos.Manager) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if err := rejectUnknownArguments(req, bindTool()); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		repoName := req.GetString("repo", "")
		lensName := req.GetString("lens", "")
		experiment := req.GetString("experiment", "")

		var (
			b     *repos.Binding
			pin   string
			mount string // "" on the unscoped endpoint
			err   error
		)
		if repos.SessionScoped(ctx) {
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
			b, pin, err = bindTarget(mgr, repoName, lensName)
		} else {
			b, pin, mount, err = scopedBindTarget(ctx, mgr, repoName, lensName)
		}
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}

		store := mgr.ClientSessions()
		if store == nil {
			return mcpgo.NewToolResultError(
				"session store unavailable — this server cannot mint a binding handle right now"), nil
		}
		// No MCP session id is read, and none is needed. The handle is the key,
		// which is exactly why two callers sharing one session id no longer
		// collide.

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
		handle, err := clientsessions.NewBindingHandle()
		if err != nil {
			log.Error().Err(err).Msg("knomit_bind: minting a handle failed")
			return mcpgo.NewToolResultError(
				"a binding handle could not be generated, so nothing was bound; ask the operator to check the server log"), nil
		}
		// No branch: knomit_bind does not take one yet, and "" already means
		// the target's own read branch. The column is carried so the planned
		// per-handle branch switch is a behaviour change, not a migration.
		if mount == "" {
			err = store.MintBindingHandle(ctx, handle, pin, "", time.Now())
		} else {
			err = store.MintBindingHandleOnMount(ctx, handle, pin, mount, time.Now())
		}
		if err != nil {
			// Worth a log line: the likely cause is a control.db that never got
			// its migrations, and without server-side evidence that presents
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
		if mount != "" {
			// A scoped connection already received this base's instructions at
			// initialize; only the handle is new.
			return mcpgo.NewToolResultText(string(out) + "\n\n" + scopedHandleBanner(handle)), nil
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

// scopedBindTarget is bindTarget for a URL-scoped endpoint: the URL's own repo
// or lens at its default branch, plus the mount the handle is recorded against.
//
// It resolves the target by the URL's NAME through bindTarget, so the binding a
// scoped handle starts on is built by exactly the code the unscoped endpoint
// uses for the same repo or lens.
func scopedBindTarget(ctx context.Context, mgr *repos.Manager, repoName, lensName string) (*repos.Binding, string, string, error) {
	kind, name := urlTarget(ctx)
	// Naming the URL's own repo or lens is harmless and accepted. Naming
	// anything else is the one thing this endpoint cannot do.
	if (repoName != "" && (kind != "repo" || repoName != name)) ||
		(lensName != "" && (kind != "lens" || lensName != name)) {
		flag, want := "--repo", repoName
		other := "repo"
		if lensName != "" {
			flag, want, other = "--lens", lensName, "lens"
		}
		return nil, "", "", fmt.Errorf(
			"This connection is fixed to %s %s; knomit_bind here only changes the branch. "+
				"To use another %s, the user must restart the bridge with %s %s. Do not retry.",
			kind, name, other, flag, want)
	}
	_, mount, plain := repos.MountSessionFromContext(ctx)
	if !plain {
		return nil, "", "", fmt.Errorf(
			"This connection is fixed to %s %s at branch %q by its URL, and that branch cannot be changed here. "+
				"Use the tools without `binding`. Do not retry.", kind, name, fixedBranch(ctx))
	}
	if mgr == nil {
		return nil, "", "", errStoreUnavailable
	}
	var (
		b   *repos.Binding
		pin string
		err error
	)
	if kind == "lens" {
		b, pin, err = bindTarget(mgr, "", name)
	} else {
		b, pin, err = bindTarget(mgr, name, "")
	}
	if err != nil {
		return nil, "", "", err
	}
	// The name resolved to the same uid the URL did, unless a rename raced
	// this call. Refused then: the handle would name a base the URL does not.
	if pin != mount {
		return nil, "", "", fmt.Errorf("%s %q changed while binding — retry", kind, name)
	}
	return b, pin, mount, nil
}

// scopedHandleBanner is handleBanner for a URL-scoped endpoint, where the
// handle is optional and chooses only the branch.
func scopedHandleBanner(handle string) string {
	return "## Your binding handle\n\n`" + handle + "`\n\n" +
		"Pass this value as the `binding` argument on the calls that should work on this handle's branch " +
		"(and on knomit_experiment, to open or leave an experiment for this handle alone). Calls without " +
		"`binding` keep using this connection's own repo or lens. Copy it verbatim; it works only on this " +
		"connection.\n"
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

	exp, ok, err := svc.GetExperiment(ctx, experiment)
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
