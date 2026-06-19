package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

// writeJSON encodes v as JSON and writes it to w with Content-Type: application/json.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
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
func validateURLAuth(u, authMethod string) error {
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

// localOriginPath reports whether s denotes a local filesystem origin — a bare
// absolute path or a file:// URL — and returns the path it refers to. Every
// other remote shape (https://, ssh://, git://, scp-style git@host:path)
// returns ("", false).
func localOriginPath(s string) (string, bool) {
	if rest, ok := strings.CutPrefix(s, "file://"); ok {
		// file:///srv/kb → /srv/kb. Tolerate a host component by preferring
		// the parsed Path; fall back to the raw remainder if parsing fails.
		if u, err := url.Parse(s); err == nil && u.Path != "" {
			return u.Path, true
		}
		return rest, true
	}
	if filepath.IsAbs(s) {
		return s, true
	}
	return "", false
}

// validateLocalOrigin enforces the local-origin policy. Network origins pass
// through untouched. Local-filesystem origins (bare absolute paths or file://
// URLs) are permitted only when localOriginRoot is configured AND the origin
// resolves to a path within that root — otherwise the server could be steered
// to clone arbitrary repos off its own disk. An empty localOriginRoot disables
// local origins entirely.
func validateLocalOrigin(s, localOriginRoot string) error {
	path, ok := localOriginPath(s)
	if !ok {
		return nil
	}
	if localOriginRoot == "" {
		return fmt.Errorf("local-path origins are disabled — set local_origin_root (or KNOMIT_LOCAL_ORIGIN_ROOT) to allow them")
	}
	root := filepath.Clean(localOriginRoot)
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("local origin %q is outside the allowed root %q", path, root)
	}
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
