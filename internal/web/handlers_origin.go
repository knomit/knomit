package web

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/rs/zerolog/log"
)

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
		if req.URL == "" {
			writeError(w, http.StatusBadRequest, "url is required")
			return
		}
		if _, err := url.Parse(req.URL); err != nil {
			writeError(w, http.StatusBadRequest, "invalid url")
			return
		}

		// Save remote config — sync loops will pick it up on next restart.
		branch := "main"
		interval := 300
		pushInterval := 300
		if err := ri.Svc.SetRemote("origin", req.URL, branch, interval, pushInterval); err != nil {
			log.Warn().Err(err).Str("repo", ri.Name).Msg("set origin failed")
			writeError(w, http.StatusInternalServerError, "failed to save origin")
			return
		}

		log.Info().Str("repo", ri.Name).Str("url", req.URL).Msg("origin configured (restart to activate sync)")

		// Return the saved remote.
		remote, err := ri.Svc.GetRemote("origin")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "saved but failed to read back")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok",
			"branch": remote.Branch,
			"head":   "",
		})
	}
}
