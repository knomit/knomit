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
	binding := ""
	if b, ok := repos.BindingFromContextOpt(ctx); ok {
		binding = b.PinID()
	}
	ci := req.Params.ClientInfo
	if err := store.SetClientInfo(ctx, sess.SessionID(), binding, ci.Name, ci.Version, time.Now()); err != nil {
		log.Warn().Err(err).Str("mcp_session", sess.SessionID()).Msg("client sessions: record client info failed")
	}
}
