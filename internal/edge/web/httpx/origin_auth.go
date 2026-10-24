package httpx

import (
	"net/url"
	"path/filepath"
	"strings"

	"knomit/internal/corpus/repos"
	"knomit/internal/foundation/pki"
)

// IsGitURL returns true if s is a valid git remote URL.
// Accepts standard URLs (https://, ssh://, git://), SCP-style (git@host:path),
// and bare absolute filesystem paths (local origins). Relative paths are
// rejected because they would resolve against the server's working directory.
func IsGitURL(s string) bool {
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

// ValidateURLAuth checks that the auth method is compatible with the URL scheme.
//
// Another knomit instance of this fleet is named by its plain https URL;
// knomit decides that it is one (repos.Manager.ResolveOrigin) and stores auth
// method cert for it. So cert is never an input here, and a URL under the
// fleet transport's internal scheme is refused (pki.GitURL, which reads the
// scheme case-insensitively, as go-git does). The wizard's urlAuthMismatch
// (web/src/originAuth.ts) makes the same refusals in the same words, and both
// tests run one table.
func ValidateURLAuth(u, authMethod string) error {
	if _, err := pki.GitURL(u, false); err != nil {
		return pki.ErrUnsupportedOriginScheme
	}
	if authMethod == "cert" {
		return repos.ErrCertNotAnInput
	}
	if err := repos.MethodMismatch(u, authMethod); err != nil {
		return err
	}
	// Note: SSH URL + "none" is intentionally NOT rejected. "none" is a
	// deliberate force-anonymous override; blocking it here would defeat its
	// purpose. The wizard surfaces a non-blocking advisory instead, and the
	// connectivity test reports the real failure if the host needs credentials.
	return nil
}

// AssembleAuthToken returns the appropriate auth token value from the given credentials.
func AssembleAuthToken(authMethod, token, user, password string) string {
	if authMethod == "basic" && user != "" {
		return user + ":" + password
	}
	return token
}
