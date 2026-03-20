package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// createSessionRequest is the expected JSON body for POST /origin/session.
type createSessionRequest struct {
	URL        string `json:"url"`
	AuthMethod string `json:"auth_method"`
	Token      string `json:"token"`
	User       string `json:"user"`
	Password   string `json:"password"`
}

func (rm *RepoManager) handleCreateSession(sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")

		var req createSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		if req.URL == "" {
			writeError(w, http.StatusBadRequest, "url is required")
			return
		}
		if !isGitURL(req.URL) {
			writeError(w, http.StatusBadRequest, "invalid url")
			return
		}

		// Validate URL/auth compatibility.
		isSSHURL := strings.HasPrefix(req.URL, "git@") || strings.HasPrefix(req.URL, "ssh://")
		isHTTPURL := strings.HasPrefix(req.URL, "http://") || strings.HasPrefix(req.URL, "https://")
		if isHTTPURL && req.AuthMethod == "ssh" {
			writeError(w, http.StatusBadRequest, "SSH auth cannot be used with HTTP/HTTPS URLs — use a token or basic auth instead")
			return
		}
		if isSSHURL && (req.AuthMethod == "token" || req.AuthMethod == "basic") {
			writeError(w, http.StatusBadRequest, "token/basic auth cannot be used with SSH URLs — use SSH auth instead")
			return
		}

		auth := AuthConfig{
			Method:   req.AuthMethod,
			Token:    req.Token,
			User:     req.User,
			Password: req.Password,
		}

		sess, err := sm.Create(repo, req.URL, auth)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{
			"session_id": sess.ID,
		})
	}
}

func (rm *RepoManager) handleGetSession(sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sess, ok := sm.Get(repo, sessionID)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}

		sess.mu.Lock()
		resp := map[string]any{
			"session_id": sess.ID,
			"state":      string(sess.State),
			"url":        sess.URL,
		}
		if sess.TestResult != nil {
			resp["history"] = sess.TestResult
		}
		if sess.PreviewResult != nil {
			resp["last_preview"] = sess.PreviewResult
		}
		if sess.ApplyResult != nil {
			resp["last_apply"] = sess.ApplyResult
		}
		sess.mu.Unlock()

		writeJSON(w, http.StatusOK, resp)
	}
}

func (rm *RepoManager) handleDeleteSession(sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sm.Delete(repo, sessionID)
		w.WriteHeader(http.StatusNoContent)
	}
}
