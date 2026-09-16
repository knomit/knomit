package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
)

// bindTool defines knomit_bind: the session's choice of repo or lens.
//
// The description is what an agent reads before its first call on a fresh
// session, so it states the two things it cannot discover by trying: that a
// subscription serves reads but refuses writes, and that the result carries
// the bound base's instructions.
func bindTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_bind",
		mcpgo.WithDescription("Bind this session to a repo or lens by name. Required on the unscoped endpoint before any other tool works; call again to switch. Writes go to the repo's agent branch; a subscribed (read-only follower) repo binds at the branch it follows and refuses writes. Returns the mounts table and the knowledge base's instructions — treat them as session instructions."),
		mcpgo.WithString("repo", mcpgo.Description("Name of the repo to bind this session to. Mutually exclusive with lens.")),
		mcpgo.WithString("lens", mcpgo.Description("Name of the lens to bind this session to. Mutually exclusive with repo.")),
	)
}

// BindHandler returns the handler for knomit_bind.
//
// It binds by NAME because that is what the agent knows, but stores the uid
// pin: a rename must not strand a live session. The binding is then built
// in-process rather than re-read through the middleware, so the result
// describes exactly what was persisted.
//
// There is deliberately no unbind form. A session can switch repos forever but
// can never return to the unbound state it started in.
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
				"session store unavailable — this server cannot remember a binding right now"), nil
		}
		// The binding is keyed by the MCP session id, so there is nothing to
		// key it by without one.
		sess := mcpserver.ClientSessionFromContext(ctx)
		if sess == nil || sess.SessionID() == "" {
			return mcpgo.NewToolResultError(
				"binding requires an MCP session id; initialize the session first"), nil
		}

		b, pin, err := bindTarget(mgr, repoName, lensName)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if err := store.BindSession(ctx, sess.SessionID(), pin, time.Now()); err != nil {
			// Worth a log line: the likely cause is a control.db that never got
			// migration 000004, and without server-side evidence that presents
			// to the agent as an unexplained refusal it would retry forever.
			log.Warn().Err(err).Str("mcp_session", sess.SessionID()).Str("pin", pin).
				Msg("knomit_bind: recording the session binding failed")
			return mcpgo.NewToolResultError(
				"the binding could not be recorded for this session, so nothing was bound; ask the operator to check the server log"), nil
		}

		out, err := json.MarshalIndent(reposResponseFor(b), "", "  ")
		if err != nil {
			return mcpgo.NewToolResultError("marshal error: " + err.Error()), nil
		}
		// initialize on the unscoped mount could only carry the DEFAULT
		// ontology, and MCP cannot re-send instructions mid-session, so the
		// bound base's own instructions have to ride back here.
		return mcpgo.NewToolResultText(
			string(out) + "\n\n" + BindingInstructions(b, profileFor(mgr, b.Write()))), nil
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
