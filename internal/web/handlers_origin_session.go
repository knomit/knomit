package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/git"
	storegit "knomit/internal/store/git"
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

// connectivityResult is the JSON payload sent in the "done" phase of a test connectivity SSE stream.
type connectivityResult struct {
	Branches        []string `json:"branches"`
	DefaultBranch   string   `json:"default_branch"`
	History         string   `json:"history"` // "shared" or "disjoint"
	RemoteFactCount int      `json:"remote_fact_count"`
	LocalFactCount  int      `json:"local_fact_count"`
}

// handleTestConnectivity handles GET /api/v1/{repo}/origin/session/{sessionID}/test
func (rm *RepoManager) handleTestConnectivity(sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sess, ok := sm.Get(repo, sessionID)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}

		ri := RepoFromContext(r.Context())

		// Set SSE headers.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		sendEvent := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		// Phase: connecting.
		sendEvent(map[string]string{"phase": "connecting"})

		// Resolve auth from session config.
		authCfg := git.RemoteAuthConfig{
			AuthMethod: sess.Auth.Method,
			Token:      sess.Auth.Token,
			User:       sess.Auth.User,
			Password:   sess.Auth.Password,
		}
		auth, err := git.ResolveAuthWithOrigin(authCfg, "", sess.URL)
		if err != nil {
			sendEvent(map[string]string{"phase": "error", "message": fmt.Sprintf("auth resolution failed: %v", err)})
			return
		}

		// Create a storer for the clone in the session temp dir.
		dbPath := filepath.Join(sess.TempDir, "clone.db")
		dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000"
		db, err := sql.Open("sqlite3", dsn)
		if err != nil {
			sendEvent(map[string]string{"phase": "error", "message": fmt.Sprintf("open clone db: %v", err)})
			return
		}
		schema := `
CREATE TABLE IF NOT EXISTS objects (hash TEXT NOT NULL, type INTEGER NOT NULL, size INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY (hash, type));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target TEXT NOT NULL, is_symbolic INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value BLOB NOT NULL);
`
		if _, err := db.Exec(schema); err != nil {
			db.Close()
			sendEvent(map[string]string{"phase": "error", "message": fmt.Sprintf("init clone schema: %v", err)})
			return
		}
		storer := storegit.NewStorer(db)

		// Phase: cloning.
		sendEvent(map[string]string{"phase": "cloning"})

		progressFn := func(msg string) {
			sendEvent(map[string]string{"phase": "cloning", "progress": msg})
		}

		cloned, err := git.CloneInto(storer, sess.URL, auth, progressFn)
		if err != nil {
			db.Close()
			log.Warn().Err(err).Str("repo", repo).Str("url", sess.URL).Msg("test connectivity: clone failed")
			sendEvent(map[string]string{"phase": "error", "message": fmt.Sprintf("clone failed: %v", err)})
			sess.mu.Lock()
			sess.State = StateError
			sess.mu.Unlock()
			return
		}

		// Phase: analyzing.
		sendEvent(map[string]string{"phase": "analyzing"})

		// Get default branch.
		defaultBranch, err := cloned.DefaultBranch()
		if err != nil {
			defaultBranch = cloned.Branch()
		}

		// List branches from the cloned store's references.
		branches := collectBranches(storer)

		// Check shared history.
		localGS, isRealStore := ri.GS.(*git.Store)
		history := "disjoint"
		if isRealStore {
			shared, err := localGS.HasSharedHistory(cloned)
			if err == nil && shared {
				history = "shared"
			}
		}

		// Count remote facts (files in the cloned store).
		remoteFiles, err := cloned.ListAll()
		remoteFactCount := 0
		if err == nil {
			remoteFactCount = len(remoteFiles)
		}

		// Count local facts.
		localFiles, err := ri.GS.ListAll()
		localFactCount := 0
		if err == nil {
			localFactCount = len(localFiles)
		}

		result := connectivityResult{
			Branches:        branches,
			DefaultBranch:   defaultBranch,
			History:         history,
			RemoteFactCount: remoteFactCount,
			LocalFactCount:  localFactCount,
		}

		// Send done event.
		sendEvent(map[string]any{"phase": "done", "result": result})

		// Update session state.
		sess.mu.Lock()
		sess.State = StateTested
		sess.TestResult = result
		sess.RemoteStore = cloned
		sess.mu.Unlock()

		log.Info().Str("repo", repo).Str("session_id", sessionID).Str("history", history).Msg("test connectivity completed")
	}
}

// collectBranches iterates references in the storer and returns branch names.
func collectBranches(s *storegit.Storer) []string {
	var branches []string
	refIter, err := s.IterReferences()
	if err != nil {
		return branches
	}
	defer refIter.Close()
	for {
		ref, err := refIter.Next()
		if err != nil {
			break
		}
		name := ref.Name()
		if name.IsBranch() {
			branches = append(branches, strings.TrimPrefix(name.String(), "refs/heads/"))
		}
	}
	return branches
}
