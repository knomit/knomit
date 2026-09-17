package web

import (
	"context"
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
	// One resolution, read once and used twice: the last-seen pin on the
	// client_sessions row, and the per-handle row in the session's binding set.
	// Reading it twice could disagree, because a task-augmented call may still
	// be writing into the recorder while this runs.
	resolved := repos.ResolvedBindingFromContext(r.Context())
	obs := sessions.Observation{
		SessionID: sid,
		Binding:   resolved.Pin,
		RemoteIP:  sessions.RemoteIP(r.RemoteAddr),
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
	// The binding SET. Only a request that actually resolved a handle adds a
	// row, so a URL-scoped request — which has a pin but no handle — records
	// its pin on the session row as before and contributes nothing here. That
	// asymmetry is correct: the set answers "which handles has this session
	// presented", and a URL-scoped caller presented none.
	if resolved.Handle != "" && resolved.Pin != "" {
		if err := store.RecordSessionBinding(ctx, sid, resolved.Handle, resolved.Pin, resolved.Branch, now); err != nil {
			log.Warn().Err(err).Str("mcp_session", sid).Msg("client sessions: binding set update failed")
		}
	}
}
