package mcp

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"knomit/internal/synthesize"
)

// actorMaxLen caps each component of the composed handle.
//
// MN13 classification: this is a DEFENSIVE RESOURCE BOUND on untrusted input,
// not a corpus property. Every byte below arrives from the caller — the session
// id is a header value and the client name/version are whatever `initialize`
// declared — so an unbounded one would ride into a DB column and into every log
// line built from it. internal/web/observability.go already caps exactly these
// fields for exactly this reason (truncateForLog); 128 is the same order of
// magnitude, comfortably above a UUID-shaped id and any plausible client name.
const actorMaxLen = 128

// actorFromRequest composes the correlation handle recorded as
// pipeline_sessions.created_by.
//
// IT IS NOT AN IDENTITY AND NOT AUTHENTICATION. The session id comes from the
// caller's own Mcp-Session-Id header and this server does not verify it —
// measured: a fabricated id that never ran `initialize` is accepted and reaches
// the handler, just with no client info attached. The MCP specification says a
// session id "is not evidence of who the caller is" and MUST NOT be treated as
// authentication. What this records is what the opening call SAID, which is
// enough to correlate an unexpected session against the HTTP log (which already
// logs the same value as `mcp_session`) and nothing more (knomit#123).
//
// The value is scheme-prefixed so the kind of claim is legible in the value
// itself rather than in the reader's assumptions:
//
//	mcp-session:<id>
//	mcp-session:<id> client:<name>/<version>
//
// The client clause appears only when the session actually initialized against
// this process. It is an enrichment; the id is the key.
//
// Returns "" when there is no MCP session at all — an in-process caller. That
// is honest absence rather than an invented "unknown": over HTTP a tools/call
// without a session id is rejected before any handler runs, so on that path the
// id is always present.
func actorFromRequest(ctx context.Context, req mcpgo.CallToolRequest) string {
	sess := mcpserver.ClientSessionFromContext(ctx)
	if sess == nil {
		return ""
	}
	// Prefer the session object over req.Header: it is what the server
	// dispatched this call under, so the two can never disagree about which
	// session the row is attributed to.
	id := sanitizeActorPart(sess.SessionID())
	if id == "" {
		return ""
	}
	out := "mcp-session:" + id

	if info, ok := sess.(mcpserver.SessionWithClientInfo); ok {
		ci := info.GetClientInfo()
		name := sanitizeActorPart(ci.Name)
		if name != "" {
			out += " client:" + name
			if ver := sanitizeActorPart(ci.Version); ver != "" {
				out += "/" + ver
			}
		}
	}
	return out
}

// sanitizeActorPart makes one caller-supplied component safe to store and to
// log: control characters (newlines above all — a log line is one line) and
// spaces are dropped, since space separates the clauses of the composed value,
// and the result is capped. Returns "" for a component with nothing left.
func sanitizeActorPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > actorMaxLen {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// withActor attaches the composed handle to ctx for Pipeline.StartSession to
// bind onto the session row. Called by the two handlers that open pipeline
// sessions (knomit_review and knomit_hypothesize); both drive the same engine.
func withActor(ctx context.Context, req mcpgo.CallToolRequest) context.Context {
	return synthesize.WithActor(ctx, actorFromRequest(ctx, req))
}
