package web

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/client/sessions"
	"knomit/internal/repos"
)

// recordClientSession is the MCP dispatch's one call into the client-session
// registry. It sees EVERY request — including ones on session ids this
// process never minted, which the default mcp-go manager accepts after a
// restart — so it, and not the initialize hook, is the recording point.
// Errors are logged and dropped; store == nil is a no-op.
func recordClientSession(r *http.Request, store *sessions.Store) {
	if store == nil {
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		return
	}
	// DETACHED from the request context, which net/http cancels the moment the
	// client's socket closes. The bridge's clean exit is exactly that race: it
	// reads the DELETE response, exits, the socket closes, and the cancellation
	// lands while End is mid-statement — leaving ended_at NULL for every clean
	// shutdown, the one case the DELETE exists to record. The call is one short
	// statement against a local database, so it needs no deadline of its own.
	ctx := context.WithoutCancel(r.Context())
	now := time.Now()
	if r.Method == http.MethodDelete {
		if err := store.End(ctx, sid, now); err != nil {
			log.Warn().Err(err).Str("mcp_session", sid).Msg("client sessions: end failed")
		}
		return
	}
	obs := sessions.Observation{
		SessionID: sid,
		Binding:   bindingPin(r),
		RemoteIP:  remoteIP(r.RemoteAddr),
		UserAgent: r.Header.Get("User-Agent"),
		Now:       now,
	}
	if raw := r.Header.Get(sessions.ClientHeader); raw != "" {
		info, err := sessions.ParseClientHeader(raw)
		if err != nil {
			log.Debug().Err(err).Str("mcp_session", sid).
				Msg("client sessions: unparseable " + sessions.ClientHeader + "; recording as http")
		} else {
			obs.Client = &info
		}
	}
	if err := store.Touch(ctx, obs); err != nil {
		log.Warn().Err(err).Str("mcp_session", sid).Msg("client sessions: touch failed")
	}
}

// bindingPin resolves the request's binding to a PinID ("repo:<uid>" or
// "lens:<uid>"). The lens mount sets an explicit binding; the repo mount sets
// only a RepoInstance, and BindingFromContext synthesizes the lens-of-one
// from it — but it PANICS when neither is present, so the RepoInstance is
// checked first rather than trusted. "" when the route carries neither.
func bindingPin(r *http.Request) string {
	ctx := r.Context()
	if b, ok := repos.BindingFromContextOpt(ctx); ok {
		return b.PinID()
	}
	if _, ok := repos.RepoFromContextOpt(ctx); ok {
		return repos.BindingFromContext(ctx).PinID()
	}
	return ""
}

// remoteIP strips the port; a value with no port is returned as is. The port
// is deliberately never recorded: it identifies nothing about the client and
// changes on every connection.
func remoteIP(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
