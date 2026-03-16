package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/rs/zerolog/log"
)

// isGitURL returns true if s is a valid git remote URL.
// Accepts standard URLs (https://, ssh://, git://) and SCP-style (git@host:path).
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
		ri := RepoFromContext(r.Context())
		if ri.Svc == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		remote, err := ri.Svc.GetRemote("origin")
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
		ri := RepoFromContext(r.Context())
		if ri.Svc == nil {
			writeError(w, http.StatusInternalServerError, "no store available")
			return
		}

		var req setOriginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		// Load existing remote to support partial updates.
		existing, _ := ri.Svc.GetRemote("origin")

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
		authToken := req.Token
		if authMethod == "basic" && req.User != "" {
			authToken = req.User + ":" + req.Password
		}
		// If no new token provided, keep existing.
		if authToken == "" && existing != nil {
			authToken = existing.AuthToken
		}

		// Validate URL/auth compatibility.
		isSSHURL := strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://")
		isHTTPURL := strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
		if isHTTPURL && authMethod == "ssh" {
			writeError(w, http.StatusBadRequest, "SSH auth cannot be used with HTTP/HTTPS URLs — use a token or basic auth instead")
			return
		}
		if isSSHURL && (authMethod == "token" || authMethod == "basic") {
			writeError(w, http.StatusBadRequest, "token/basic auth cannot be used with SSH URLs — use SSH auth instead")
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

		if err := ri.Svc.SetRemoteWithAuth("origin", url, branch, interval, pushInterval, authMethod, authToken); err != nil {
			log.Warn().Err(err).Str("repo", ri.Name).Msg("set origin failed")
			writeError(w, http.StatusInternalServerError, "failed to save origin")
			return
		}

		// Activate sync loops if the callback is set.
		if ri.StartSync != nil {
			if err := ri.StartSync(url); err != nil {
				log.Warn().Err(err).Str("repo", ri.Name).Msg("sync activation failed")
			} else {
				log.Info().Str("repo", ri.Name).Str("url", url).Msg("origin configured and sync activated")
			}
		} else {
			log.Info().Str("repo", ri.Name).Str("url", url).Msg("origin configured (restart to activate sync)")
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok",
			"branch": branch,
		})
	}
}
