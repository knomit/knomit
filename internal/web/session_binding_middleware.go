package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
)

// peekLimit bounds how much of a request body the initialize probe reads. An
// initialize envelope is a few hundred bytes, so a body that does not fit in
// this window is by construction not an initialize.
const peekLimit = 4096

// isInitializeRequest reports whether this request's JSON-RPC body is an
// `initialize` call.
//
// The binding lookup MUST be skipped for initialize. mcp-go v0.45 mints a
// fresh session id on every initialize and ignores the inbound Mcp-Session-Id
// (server/streamable_http.go: isInitializeRequest ⇒ sessionIdManager.
// Generate()), while the bridge keeps sending its previous id once it has one.
// So during initialize the header names a DIFFERENT session than the one the
// response will carry, and anything keyed on it — the Binding, and the pin
// recordClientInfo stamps on the new session's client_sessions row — would be
// the previous session's state.
//
// This is the first read-and-restore of a request body in this package, so the
// three rules it follows are the pattern to copy:
//
//  1. POST only. A GET (the SSE stream) or DELETE never has a body to peek.
//  2. BOUNDED window. At most peekLimit bytes are buffered, and the remainder
//     still streams: the body is restored as the peeked prefix followed by the
//     untouched reader, so a large request is never held twice and nothing is
//     truncated. (http.MaxBytesReader is wrong here — the body is passed
//     downstream, not consumed.) The method is read token-wise, so a prefix
//     cut mid-body still yields it — see methodOf.
//  3. FAIL OPEN. A read error, an oversized body, or anything that does not
//     decode is simply "not an initialize" and resolution proceeds as normal.
//     Rejecting a malformed body is mcp-go's job, and only it can do so with
//     proper JSON-RPC framing.
func isInitializeRequest(r *http.Request) bool {
	if r.Method != http.MethodPost || r.Body == nil {
		return false
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, peekLimit))
	// Restore before any early return: the prefix we consumed, then whatever
	// is left of the original stream.
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
	if err != nil {
		return false
	}
	return methodOf(buf) == "initialize"
}

// methodOf pulls the top-level "method" out of a JSON-RPC envelope, reading
// TOKENS rather than unmarshalling, so it works on a prefix that was cut off
// mid-body. It returns "" when the method cannot be determined.
//
// Token-wise is what makes the bounded window safe. "method" sits within the
// first few dozen bytes of every envelope knomit sees, so the scan reaches it
// long before the cut even when the body is enormous — an unmarshal of the
// same prefix would simply fail as malformed JSON.
//
// The residue: a client could order its keys so that a large params object
// precedes "method" and pushes it past the window. That request reads as "not
// an initialize" and resolves normally. The user-visible behaviour is still
// correct, because AfterInitialize independently returns the unbound
// instructions for any session-scoped initialize; what is left is that
// recordClientInfo may stamp the previous session's pin on the new session's
// client_sessions row — an observational column that must never gate anything
// and that the session's next request overwrites.
func methodOf(buf []byte) string {
	dec := json.NewDecoder(bytes.NewReader(buf))
	t, err := dec.Token()
	if err != nil || t != json.Delim('{') {
		return ""
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return ""
		}
		if key == "method" {
			v, err := dec.Token()
			if err != nil {
				return ""
			}
			m, _ := v.(string)
			return m
		}
		if err := skipJSONValue(dec); err != nil {
			return ""
		}
	}
	return ""
}

// skipJSONValue consumes one value, descending through objects and arrays so
// the scan lands on the next key.
func skipJSONValue(dec *json.Decoder) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') && t != json.Delim('[') {
		return nil
	}
	for depth := 1; depth > 0; {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		switch t {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}

// SessionBindingMiddleware serves the unscoped /api/v1/mcp mount. It marks the
// context session-scoped and, when the request carries an Mcp-Session-Id with
// a stored binding, resolves it exactly as LensMiddleware does for a URL. A
// stored pin that no longer resolves is placed in the context as an error —
// never rendered as an HTTP failure — so the JSON-RPC framing survives and the
// tool handler is the one that tells the agent to bind again. The initialize
// request has no session id yet and passes through unbound by design.
func SessionBindingMiddleware(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := repos.WithSessionScoped(r.Context())
			// An initialize is a NEW session whatever the header says, so it
			// passes through unbound by design — see isInitializeRequest.
			sid := r.Header.Get("Mcp-Session-Id")
			store := m.ClientSessions()
			if sid != "" && store != nil && !isInitializeRequest(r) {
				pin, ok, err := store.SessionBinding(ctx, sid)
				switch {
				case err != nil:
					log.Warn().Err(err).Str("mcp_session", sid).Msg("session binding: lookup failed")
					ctx = repos.WithBindingError(ctx,
						errors.New("session binding lookup failed — retry, or call knomit_bind again"))
				case ok:
					if rctx, rerr := repos.ResolveSessionBinding(ctx, m, pin); rerr != nil {
						ctx = repos.WithBindingError(ctx, rerr)
					} else {
						ctx = rctx
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
