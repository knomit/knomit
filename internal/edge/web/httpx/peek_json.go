package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// DefaultPeekBytes bounds PeekJSON for a route whose handler sets no limit
// of its own.
const DefaultPeekBytes = 1 << 20

// PeekJSON decodes r's body into v EXACTLY as DecodeJSON would hand it to the
// handler — the same encoding/json decoder into the same type, so field names
// match case-insensitively and a duplicate key decodes into the same value
// the handler's would — and then puts the bytes back, so the handler still
// reads (and DecodeJSON still judges) the whole body.
//
// It is for a decision that must agree with the handler about the body: the
// route permission registry's resolvers (internal/edge/web route_rules.go).
// Decoding into a type of its own, or into a generic map, is how a resolver
// and a handler come to disagree, so a resolver passes the handler's own
// request struct.
//
// It reports false — and the caller must then decide the strictest way — when
// the body is longer than maxBytes (DefaultPeekBytes when maxBytes <= 0), is
// not exactly one JSON value of v's shape, or cannot be read. An empty or
// all-whitespace body leaves v zero and reports true. The Content-Type is not
// judged: the handler's DecodeJSON still refuses a non-JSON body with 415.
//
// The limit is deliberately allowed to be LOWER than the handler's: several
// handlers decode with no limit at all (DecodeJSON/DecodeOptionalJSON with
// 0), and their resolvers peek at most DefaultPeekBytes. A valid body over
// that reports false, and the resolver answers the strongest permission.
// That is intended fail-closed behaviour, not a bug: P1 is dark and changes
// no handler limit, and P4 decides whether those handlers get a real limit
// (route_rules.go's P4 TODO).
func PeekJSON(r *http.Request, v any, maxBytes int64) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	if maxBytes <= 0 {
		maxBytes = DefaultPeekBytes
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
	if err != nil || int64(len(buf)) > maxBytes {
		return false
	}
	if len(bytes.TrimSpace(buf)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	if err := dec.Decode(v); err != nil {
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return false // trailing data: DecodeJSON refuses it too
	}
	return true
}
