package oauth

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const (
	asWellKnown  = "/.well-known/oauth-authorization-server"
	prmWellKnown = "/.well-known/oauth-protected-resource"
)

// ScopesSupported are the permission names a token's ceiling may carry.
// admin is not among them: approving tokens, enrolling and changing grants
// stay with local principals.
var ScopesSupported = []string{"read", "write", "push:own", "merge:main", "operator"}

// issuerParts splits a normalised issuer (config.NormalizeIssuer) into its
// origin and its path ("" for an origin-only issuer).
func issuerParts(issuer string) (origin, p string) {
	u, err := url.Parse(issuer)
	if err != nil {
		return issuer, ""
	}
	return u.Scheme + "://" + u.Host, u.EscapedPath()
}

// WellKnown serves the two discovery documents for issuer, at the RFC 8414
// §3 / RFC 9728 §3.1 locations: the well-known segment goes BETWEEN the
// origin and any path, so for https://host/knomit they are
// /.well-known/oauth-authorization-server/knomit and
// /.well-known/oauth-protected-resource/knomit[/<resource path>]. It answers
// 404 for everything else, including the origin-root documents of an issuer
// that has a path: those belong to whoever owns the origin.
//
// The protected-resource document ECHOES the resource its URL names (RFC 9728
// §3.3: a client rejects a document whose resource differs from the URL it
// derived the metadata location from). Only canonical paths under the issuer
// qualify: no escapes (knomit's own resource URLs carry none — a branch's
// "/" is spelled ":"), no "..", no empty segments.
func WellKnown(issuer string) http.Handler {
	origin, ipath := issuerParts(issuer)
	as := asDocument(issuer)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		raw := r.URL.EscapedPath()
		switch {
		case raw == asWellKnown+ipath:
			writeJSON(w, http.StatusOK, as)
			return
		case strings.HasPrefix(raw, prmWellKnown+ipath):
			// canonicalPath is also the segment-boundary check: the suffix of
			// ".../knomitx" is "x", which does not start with "/".
			suffix := strings.TrimPrefix(raw, prmWellKnown+ipath)
			if suffix != "" && !canonicalPath(suffix) {
				break
			}
			writeJSON(w, http.StatusOK, prmDocument(issuer, origin+ipath+suffix))
			return
		}
		http.NotFound(w, r)
	})
}

// canonicalPath reports whether an escaped request path is already in its one
// spelling: absolute, no percent-escapes, and unchanged by path.Clean (so no
// ".", "..", "//" or trailing "/"). A path that is not canonical never names a
// resource: callers refuse it rather than guess what it meant.
func canonicalPath(escaped string) bool {
	if !strings.HasPrefix(escaped, "/") || strings.Contains(escaped, "%") {
		return false
	}
	return path.Clean(escaped) == escaped
}

func asDocument(issuer string) map[string]any {
	return map[string]any{
		"issuer":                                         issuer,
		"authorization_endpoint":                         issuer + "/oauth/authorize",
		"token_endpoint":                                 issuer + "/oauth/token",
		"revocation_endpoint":                            issuer + "/oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"revocation_endpoint_auth_methods_supported":     []string{"none"},
		"scopes_supported":                               ScopesSupported,
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	}
}

func prmDocument(issuer, resource string) map[string]any {
	return map[string]any{
		"resource":                 resource,
		"authorization_servers":    []string{issuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         ScopesSupported,
		"resource_name":            "knomit",
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ResourceMetadataURL is the protected-resource metadata URL for a request
// whose path, as the OAuth listener sees it, is p: the RFC 9728 path form
// for the resource issuer + p. It is what a 401's resource_metadata names,
// so a client that follows it asks for a token confined to that resource.
func ResourceMetadataURL(issuer, p string) string {
	origin, ipath := issuerParts(issuer)
	return origin + prmWellKnown + ipath + p
}
