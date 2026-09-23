package oauth

import (
	"net/url"
	"strings"
)

// resourceUnder reports whether resource is the issuer or a URL under it:
// the issuer's origin byte-for-byte, then nothing, or a canonical path (no
// escapes, no "..", no "//", no trailing "/") that equals the issuer's path
// or extends it at a "/" boundary. No userinfo, query or fragment. It is
// what a token's audience may be (R3): the root for `kb login`, one MCP
// endpoint for a client that names one.
func resourceUnder(issuer, resource string) bool {
	u, err := url.Parse(resource)
	if err != nil || !printableASCII(resource) || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(resource, "?#") {
		return false
	}
	origin, ipath := issuerParts(issuer)
	if !strings.HasPrefix(resource, origin) {
		return false
	}
	rest := resource[len(origin):]
	if rest == "" {
		return ipath == ""
	}
	if rest == "/" || !canonicalPath(rest) {
		return false
	}
	return pathUnder(rest, ipath)
}

// pathUnder reports whether p equals prefix or extends it at a "/"
// boundary; an empty prefix contains everything. Both must already be
// canonical: this compares strings, it does not clean them.
func pathUnder(p, prefix string) bool {
	return prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/")
}
