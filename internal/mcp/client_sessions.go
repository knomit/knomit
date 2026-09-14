package mcp

import (
	"context"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

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
	if err := store.SetClientInfo(ctx, sess.SessionID(), repos.BindingPinFromContext(ctx), ci.Name, ci.Version, time.Now()); err != nil {
		log.Warn().Err(err).Str("mcp_session", sess.SessionID()).Msg("client sessions: record client info failed")
	}
}
