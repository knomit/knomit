package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/rs/zerolog/log"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// isGitURL returns true if s is a valid git remote URL.
// Accepts standard URLs (https://, ssh://, git://) and SCP-style (git@host:path).
// validateURLAuth checks that the auth method is compatible with the URL scheme.
func validateURLAuth(url, authMethod string) error {
	isSSH := strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://")
	isHTTP := strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
	if isHTTP && authMethod == "ssh" {
		return fmt.Errorf("SSH auth cannot be used with HTTP/HTTPS URLs — use a token or basic auth instead")
	}
	if isSSH && (authMethod == "token" || authMethod == "basic") {
		return fmt.Errorf("token/basic auth cannot be used with SSH URLs — use SSH auth instead")
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

func isGitURL(s string) bool {
	if strings.Contains(s, "://") {
		_, err := url.Parse(s)
		return err == nil
	}
	// SCP-style: user@host:path
	at := strings.Index(s, "@")
	colon := strings.Index(s, ":")
	return at > 0 && colon > at && colon < len(s)-1
}

func handleGetOrigin() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		var svc *store.Service
		ri.WithRead(func(d repos.StoreDeps) { svc = d.Svc })
		if svc == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		remote, err := svc.GetRemote("origin")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if remote == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Never send credentials to the browser.
		remote.AuthToken = ""
		writeJSON(w, http.StatusOK, remote)
	}
}

// setOriginRequest is the expected JSON body for PUT /origin.
type setOriginRequest struct {
	URL        string `json:"url"`
	AuthMethod string `json:"auth_method"`
	Token      string `json:"token"`
	User       string `json:"user"`
	Password   string `json:"password"`
}

func handleSetOrigin() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		var svc *store.Service
		ri.WithRead(func(d repos.StoreDeps) { svc = d.Svc })
		repoName := ri.Name()

		if svc == nil {
			writeError(w, http.StatusInternalServerError, "no store available")
			return
		}

		var req setOriginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		// Load existing remote to support partial updates.
		existing, _ := svc.GetRemote("origin")

		// Resolve URL: use request value, fall back to existing.
		url := req.URL
		if url == "" && existing != nil {
			url = existing.URL
		}
		if url == "" {
			writeError(w, http.StatusBadRequest, "url is required")
			return
		}
		if req.URL != "" && !isGitURL(req.URL) {
			writeError(w, http.StatusBadRequest, "invalid url")
			return
		}

		// Resolve auth: use request values, fall back to existing.
		authMethod := req.AuthMethod
		if authMethod == "" && existing != nil {
			authMethod = existing.AuthMethod
		}
		authToken := assembleAuthToken(authMethod, req.Token, req.User, req.Password)
		// If no new token provided, keep existing.
		if authToken == "" && existing != nil {
			authToken = existing.AuthToken
		}

		// Validate URL/auth compatibility.
		if err := validateURLAuth(url, authMethod); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		// Preserve existing intervals or use defaults.
		branch := "main"
		interval := 300
		pushInterval := 300
		if existing != nil {
			branch = existing.Branch
			interval = existing.Interval
			pushInterval = existing.PushInterval
		}

		if err := svc.SetRemoteWithAuth("origin", url, branch, interval, pushInterval, authMethod, authToken); err != nil {
			log.Warn().Err(err).Str("repo", repoName).Msg("set origin failed")
			writeError(w, http.StatusInternalServerError, "failed to save origin")
			return
		}

		// Activate sync loops.
		if err := ri.ActivateSync(url); err != nil {
			log.Warn().Err(err).Str("repo", repoName).Msg("sync activation failed")
		} else {
			log.Info().Str("repo", repoName).Str("url", url).Msg("origin configured and sync activated")
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok",
			"branch": branch,
		})
	}
}
