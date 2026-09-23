package web

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"knomit/internal/pki"
)

// detailWithoutTitlePrefix renders err as a problem+json `detail`, dropping a
// leading sentinel prefix that the problem's TITLE already carries.
//
// Named for what it does rather than `problemDetail`, which this package's
// lens tests already use for reading a detail back OUT of a response.
//
// A wrapped sentinel reads well in a log — "origin not allowed: local origin
// %q is outside the allowed root %q" — and badly in a problem document, where
// the title is already "Origin not allowed" and the reader gets the same
// phrase twice before reaching the part that tells them which path and which
// root. The title says WHAT KIND of refusal; the detail should say only which
// one.
//
// Prefix-stripping rather than a second message on the error, so there stays
// exactly ONE wording to keep correct. An err that does not carry the prefix
// is returned unchanged, so this is safe to apply wherever the sentinel is
// merely possible.
func detailWithoutTitlePrefix(err error, sentinel error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if sentinel == nil {
		return msg
	}
	return strings.TrimPrefix(msg, sentinel.Error()+": ")
}

// isGitURL returns true if s is a valid git remote URL.
// Accepts standard URLs (https://, ssh://, git://), SCP-style (git@host:path),
// and bare absolute filesystem paths (local origins). Relative paths are
// rejected because they would resolve against the server's working directory.
func isGitURL(s string) bool {
	if strings.Contains(s, "://") {
		_, err := url.Parse(s)
		return err == nil
	}
	// Bare absolute filesystem path → local origin.
	if filepath.IsAbs(s) {
		return true
	}
	// SCP-style: user@host:path
	at := strings.Index(s, "@")
	colon := strings.Index(s, ":")
	return at > 0 && colon > at && colon < len(s)-1
}

// validateURLAuth checks that the auth method is compatible with the URL scheme.
//
// knomit+https (another enrolled instance) authenticates ONLY with the
// instance certificate: auth method cert, or "" which resolves to it. The
// wizard's urlAuthMismatch (web/src/originAuth.ts) makes the same two
// refusals in the same words, and both tests run one table. The repos layer
// (resolveAuthWithOrigin) forces cert again for every clone and sync; this
// edge check is the one that tells the user what to change.
func validateURLAuth(u, authMethod string) error {
	isKnomit := pki.IsFleetURL(u)
	if isKnomit && authMethod != "cert" && authMethod != "" {
		return fmt.Errorf("knomit+https origins authenticate with the instance certificate — use auth method cert")
	}
	if !isKnomit && authMethod == "cert" {
		return fmt.Errorf("cert auth is only valid with knomit+https:// URLs")
	}
	isSSH := strings.HasPrefix(u, "git@") || strings.HasPrefix(u, "ssh://")
	isHTTP := strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
	if isHTTP && authMethod == "ssh" {
		return fmt.Errorf("SSH auth cannot be used with HTTP/HTTPS URLs — use a token or basic auth instead")
	}
	if isSSH && (authMethod == "token" || authMethod == "basic") {
		return fmt.Errorf("token/basic auth cannot be used with SSH URLs — use SSH auth instead")
	}
	// Note: SSH URL + "none" is intentionally NOT rejected. "none" is a
	// deliberate force-anonymous override; blocking it here would defeat its
	// purpose. The wizard surfaces a non-blocking advisory instead, and the
	// connectivity test reports the real failure if the host needs credentials.
	return nil
}

// assembleAuthToken returns the appropriate auth token value from the given credentials.
func assembleAuthToken(authMethod, token, user, password string) string {
	if authMethod == "basic" && user != "" {
		return user + ":" + password
	}
	return token
}

// setOriginRequest is the expected JSON body for PUT /repos/{repo}/origin.
//
// Branch is the upstream consensus branch on the remote (e.g. "main",
// "master"). When omitted, the handler keeps the existing remote's value or
// falls back to "main". Callers that have already discovered the right name
// (via the connectivity-test flow) should send it explicitly.
type setOriginRequest struct {
	URL        string `json:"url"`
	Branch     string `json:"branch,omitempty"`
	AuthMethod string `json:"auth_method"`
	Token      string `json:"token"`
	User       string `json:"user"`
	Password   string `json:"password"`
}
