package mcp

import (
	"context"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"knomit/internal/client/sessions"
	"knomit/internal/repos"
)

// recordClientInfo enriches the client_sessions row with what `initialize`
// declared. This is the ONLY place the host's client name/version exists;
// everything else about a session rides on request headers and is recorded
// by the HTTP dispatch layer (internal/web). Never fails the initialize:
// errors are logged and dropped. nil manager / nil store ⇒ no-op.
func recordClientInfo(ctx context.Context, mgr *repos.Manager, req *mcpgo.InitializeRequest) {
	if mgr == nil {
		return
	}
	store := mgr.ClientSessions()
	if store == nil {
		return
	}
	sess := mcpserver.ClientSessionFromContext(ctx)
	if sess == nil || sess.SessionID() == "" {
		return
	}
	// The repo-scoped route carries only a RepoInstance, so the plain Opt
	// accessor would record an empty binding for the single-repo path — the
	// common one. Shared with the HTTP dispatch recorder.
	ci := req.Params.ClientInfo
	// What the server observed about THIS request. For a direct HTTP client
	// this is the only chance to derive its identity correctly: no Touch has
	// run yet, so the row carries no ip or User-Agent to read back.
	obs := sessions.HTTPInfoFromContext(ctx)
	if err := store.SetClientInfo(ctx, sess.SessionID(), repos.BindingPinFromContext(ctx),
		ci.Name, ci.Version, obs.RemoteIP, obs.UserAgent, time.Now()); err != nil {
		log.Warn().Err(err).Str("mcp_session", sess.SessionID()).Msg("client sessions: record client info failed")
	}
}
