package web

import "strings"

// trimOriginURL strips surrounding whitespace from a user-supplied origin URL.
//
// People paste. A URL copied out of a browser, a chat message or a terminal
// routinely carries a leading or trailing space, a tab, or a newline, and none
// of them are part of the address. Nothing downstream removes them: the URL
// goes into transport.NewEndpoint as typed, and a trailing space arrives at the
// server percent-encoded in the path —
// `GET /git/arxiv-kb%20%20/info/refs` → 404 — which the wizard then reports as
// "repository not found". The user is looking at a URL that is correct except
// for something invisible, and is told the repository does not exist.
//
// It is applied at the JSON DECODE seam of every entry point that accepts an
// origin URL, immediately after decoding and before anything reads the field.
// That ordering is the point, not a detail: the URL is gated by
// validateLocalOrigin, used as the identity key that decides whether this
// knowledge base is already checked out, and then fetched. All three must see
// the SAME string, or a URL could clear the gate in one form and be used in
// another (kb/invariants/repos/remote/local-origin-gate — no trusted
// exemption). Trimming at the seam is what makes the value the handler
// validates and the value it uses one value.
//
// Whitespace ONLY. It does not lower-case, strip a trailing slash, add or
// remove a ".git" suffix, or resolve anything: those change which repository
// is named, and a create keyed on the result would silently point somewhere
// else. Tolerating the suffix and the trailing slash is the git ROUTE's job
// (gitremote.go), on the serving side, where no identity is being recorded.
func trimOriginURL(u string) string { return strings.TrimSpace(u) }
