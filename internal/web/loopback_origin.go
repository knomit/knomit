package web

import (
	"net/http"
	"net/url"
	"strings"
)

// loopbackOriginOK reports whether a request that is about to become the
// anonymous principal may do so, judged by its Origin header (#287).
//
// A cross-site page can make a browser POST to http://127.0.0.1:<port> with a
// CORS "simple" request — a form, or fetch with text/plain — and no preflight.
// It carries a legitimate loopback Host, so loopbackHostOK admits it, and
// before #287 it ran as anonymous@none with every loopback_default
// permission. What the browser cannot forge is Origin: it names the page.
//
// The rule, for any method other than GET, HEAD or OPTIONS:
//   - no Origin header at all passes. curl, the CLI, the bridge and every
//     other non-browser client send none, and a browser always sends one on a
//     non-GET request;
//   - an Origin whose scheme is http or https and whose host[:port] equals
//     r.Host (case-insensitively, the scheme's default port ignored on both
//     sides) passes: the page this listener served, or a proxy in front of it
//     presenting https://<Host>. vite's dev proxy forwards Host localhost:5173
//     with Origin http://localhost:5173, so this compares against the Host the
//     request carries, never the arrival port;
//   - an Origin in trusted — the CORS allowlist, i.e. the desktop's Wails
//     origins — passes, compared byte-exactly as corsMiddleware does;
//   - anything else is refused: "null" (present, and not ours), a malformed
//     Origin, one with userinfo, a path (even "/"), a query or a fragment, and
//     a repeated Origin header. A browser serializes none of those.
//
// GET and HEAD are exempt because browsers omit Origin on same-origin GETs and
// reads are the Host check's business (#281). OPTIONS is exempt because
// AuthMiddleware runs before corsMiddleware on the outer router, and the
// desktop's own preflights must reach it; a preflight changes nothing.
//
// This is NOT authentication. It says "a page on another origin is not asking";
// any local process can still send whatever headers it likes, exactly as
// before.
func loopbackOriginOK(r *http.Request, trusted []string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	values, present := r.Header["Origin"]
	if !present {
		return true
	}
	if len(values) != 1 {
		return false
	}
	origin := values[0]
	for _, t := range trusted {
		if origin == t {
			return true
		}
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if r.Host == "" {
		return false
	}
	return strings.EqualFold(withoutDefaultPort(u.Host, u.Scheme), withoutDefaultPort(r.Host, u.Scheme))
}

// withoutDefaultPort drops the scheme's default port from a host[:port], so
// Origin http://localhost matches Host localhost:80 and the reverse. A browser
// never serializes the default port into Origin, but a client or proxy may put
// it in Host. IPv6 literals keep their brackets: "[::1]:80" becomes "[::1]".
func withoutDefaultPort(hostport, scheme string) string {
	def := ":80"
	if scheme == "https" {
		def = ":443"
	}
	return strings.TrimSuffix(hostport, def)
}
