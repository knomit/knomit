package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"

	"knomit/internal/git"
	"knomit/internal/repos"
	"knomit/internal/store"
	storegit "knomit/internal/store/git"
)

// storeFactIterAdapter wraps store.FactsIter to implement git.FactIter.
type storeFactIterAdapter struct {
	inner *store.FactsIter
}

func (a *storeFactIterAdapter) Next() (*git.FactRow, error) {
	row, err := a.inner.Next()
	if err != nil || row == nil {
		return nil, err
	}
	return &git.FactRow{Path: row.Path, BlobHash: row.BlobHash, CommitHash: row.CommitHash}, nil
}

func (a *storeFactIterAdapter) Close() error { return a.inner.Close() }

// createSessionRequest is the expected JSON body for POST /origin/session.
type createSessionRequest struct {
	URL        string `json:"url"`
	AuthMethod string `json:"auth_method"`
	Token      string `json:"token"`
	User       string `json:"user"`
	Password   string `json:"password"`
}

func handleCreateSession(rm *repos.Manager, sm *SessionManager) http.HandlerFunc {
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
		if err := validateURLAuth(req.URL, req.AuthMethod); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
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

func handleGetSession(rm *repos.Manager, sm *SessionManager) http.HandlerFunc {
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

func handleDeleteSession(rm *repos.Manager, sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sm.Delete(repo, sessionID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// connectivityResult is the JSON payload sent in the "done" phase of a test connectivity SSE stream.
type connectivityResult struct {
	Branches        []string `json:"branches"`        // non-agent branches (selectable as main)
	AgentBranches   []string `json:"agent_branches"`  // all agent/* branches on remote
	DefaultBranch   string   `json:"default_branch"`
	MatchedAgent    string   `json:"matched_agent,omitempty"` // agent branch matching our hostname (if any)
	History         string   `json:"history"`                 // "shared" or "disjoint"
	RemoteFactCount int      `json:"remote_fact_count"`
	LocalFactCount  int      `json:"local_fact_count"`
}

// handleTestConnectivity handles GET /api/v1/{repo}/origin/session/{sessionID}/test
func handleTestConnectivity(rm *repos.Manager, sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sess, ok := sm.Get(repo, sessionID)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}

		ri := repos.RepoFromContext(r.Context())
		ri.RLock()
		gs := ri.GS
		ri.RUnlock()

		sendEvent, ok := beginSSE(w)
		if !ok {
			return
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
		if _, err := db.ExecContext(r.Context(), schema); err != nil {
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

		// Collect all branch info in a single pass over refs.
		localAgentBranch := gs.Branch()
		branches, agentBranches, matchedAgent := collectAllBranchInfo(storer, localAgentBranch)

		// Check shared history.
		localGS, isRealStore := gs.(*git.Store)
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
		localFiles, err := gs.ListAll()
		localFactCount := 0
		if err == nil {
			localFactCount = len(localFiles)
		}

		result := connectivityResult{
			Branches:      branches,
			AgentBranches: agentBranches,
			DefaultBranch: defaultBranch,
			MatchedAgent:  matchedAgent,
			History:             history,
			RemoteFactCount:     remoteFactCount,
			LocalFactCount:      localFactCount,
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

// previewResult is the JSON payload sent in the "done" phase of a preview SSE stream.
type previewResult struct {
	LocalOnly     int `json:"local_only"`
	RemoteOnly    int `json:"remote_only"`
	SharedPath    int `json:"shared_path"`
	DeadRefsFound int `json:"dead_refs_found"`
}

// handlePreview handles GET /api/v1/{repo}/origin/session/{sessionID}/preview
func handlePreview(rm *repos.Manager, sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sess, ok := sm.Get(repo, sessionID)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}

		sess.mu.Lock()
		state := sess.State
		remoteStore := sess.RemoteStore
		sess.mu.Unlock()

		if state != StateTested && state != StatePreviewed && state != StateApplied {
			writeError(w, http.StatusConflict, "session must be in tested state or later")
			return
		}
		if remoteStore == nil {
			writeError(w, http.StatusConflict, "session has no remote store (run test first)")
			return
		}

		ri := repos.RepoFromContext(r.Context())
		ri.RLock()
		svc, gs := ri.Svc, ri.GS
		ri.RUnlock()

		sendEvent, ok := beginSSE(w)
		if !ok {
			return
		}

		sendEvent(map[string]string{"phase": "comparing"})

		// Build local path set via FactsIter.
		var localDB *sql.DB
		if svc != nil {
			localDB = svc.DB()
		}

		localPaths := make(map[string]struct{})
		if localDB != nil {
			iter, err := store.NewFactsIter(localDB)
			if err != nil {
				log.Warn().Err(err).Str("repo", repo).Msg("preview: open facts iter")
			} else {
				for {
					row, err := iter.Next()
					if err != nil || row == nil {
						break
					}
					localPaths[row.Path] = struct{}{}
				}
				iter.Close()
			}
		}

		// List remote paths.
		remotePaths := make(map[string]struct{})
		remoteFiles, err := remoteStore.ListAll()
		if err != nil {
			log.Warn().Err(err).Str("repo", repo).Msg("preview: list remote")
		} else {
			for _, p := range remoteFiles {
				remotePaths[p] = struct{}{}
			}
		}

		// Compute counts.
		var localOnly, remoteOnly, shared int
		for p := range localPaths {
			if _, inRemote := remotePaths[p]; inRemote {
				shared++
			} else {
				localOnly++
			}
		}
		for p := range remotePaths {
			if _, inLocal := localPaths[p]; !inLocal {
				remoteOnly++
			}
		}

		// Dead ref detection: read local facts in parallel (bounded concurrency).
		// readMu is a conservative guard because *gogit.Repository is not documented
		// as goroutine-safe for concurrent reads. Read paths appear stateless in
		// practice but we serialize to be safe until confirmed otherwise. Note also
		// that for :memory: SQLite test databases, concurrent *sql.DB connections
		// each see a fresh empty DB.
		const workers = 8
		jobs := make(chan string, len(localPaths))
		results := make(chan int, len(localPaths))
		var readMu sync.Mutex

		for i := 0; i < workers; i++ {
			go func() {
				for p := range jobs {
					readMu.Lock()
					content, err := gs.ReadFile(p)
					readMu.Unlock()
					if err != nil {
						results <- 0
						continue
					}
					dead := 0
					for _, ref := range extractRefsFromFrontmatter(content) {
						if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
							continue
						}
						if _, alive := localPaths[ref]; !alive {
							dead++
						}
					}
					results <- dead
				}
			}()
		}
		for p := range localPaths {
			jobs <- p
		}
		close(jobs)
		deadRefs := 0
		for range localPaths {
			deadRefs += <-results
		}

		result := previewResult{
			LocalOnly:     localOnly,
			RemoteOnly:    remoteOnly,
			SharedPath:    shared,
			DeadRefsFound: deadRefs,
		}

		sendEvent(map[string]any{"phase": "done", "result": result})

		sess.mu.Lock()
		sess.State = StatePreviewed
		sess.PreviewResult = result
		sess.mu.Unlock()

		log.Info().Str("repo", repo).Str("session_id", sessionID).
			Int("local_only", localOnly).Int("remote_only", remoteOnly).
			Int("shared", shared).Int("dead_refs", deadRefs).
			Msg("preview completed")
	}
}

// applyRequest is the expected JSON body for POST /origin/session/{sessionID}/apply.
type applyRequest struct {
	ConflictStrategy string `json:"conflict_strategy"`
	Branch           string `json:"branch,omitempty"` // remote branch to track; defaults to test result's default_branch
}

// applyResult is the JSON payload sent in the "done" phase of an apply SSE stream.
type applyResult struct {
	TotalFacts           int `json:"total_facts"`
	FromLocal            int `json:"from_local"`
	FromRemote           int `json:"from_remote"`
	Overwrites           int `json:"overwrites"`
	RefsResolvedFromHist int `json:"refs_resolved_from_history"`
	DanglingRefsDropped  int `json:"dangling_refs_dropped"`
}

// handleApply handles POST /api/v1/{repo}/origin/session/{sessionID}/apply
func handleApply(rm *repos.Manager, sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sess, ok := sm.Get(repo, sessionID)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}

		sess.mu.Lock()
		state := sess.State
		remoteStore := sess.RemoteStore
		testResult := sess.TestResult
		sess.mu.Unlock()

		if state != StateTested && state != StatePreviewed && state != StateApplied {
			writeError(w, http.StatusConflict, "session must be tested before applying")
			return
		}
		if remoteStore == nil {
			writeError(w, http.StatusConflict, "session has no remote store (run test first)")
			return
		}

		var req applyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		strategy := git.ConflictStrategy(req.ConflictStrategy)
		if strategy != git.StrategyLocalWins && strategy != git.StrategyRemoteWins {
			writeError(w, http.StatusBadRequest, "conflict_strategy must be local_wins or remote_wins")
			return
		}

		// Extract test result to get history type and default branch.
		tr, ok := testResult.(connectivityResult)
		if !ok {
			writeError(w, http.StatusConflict, "session test result missing or invalid")
			return
		}

		// Resolve the remote branch to track: explicit request > test result default.
		remoteBranch := tr.DefaultBranch
		if req.Branch != "" {
			remoteBranch = req.Branch
		}

		ri := repos.RepoFromContext(r.Context())
		ri.RLock()
		svc, gs := ri.Svc, ri.GS
		ri.RUnlock()

		sendEvent, ok := beginSSE(w)
		if !ok {
			return
		}

		if tr.History == "disjoint" {
			sendEvent(map[string]string{"phase": "replaying"})

			// Get the local store and local DB for replay.
			localGS, isRealStore := gs.(*git.Store)
			if !isRealStore {
				sendEvent(map[string]string{"phase": "error", "message": "local store is not a git store"})
				return
			}

			var localDB *sql.DB
			if svc != nil {
				localDB = svc.DB()
			}
			if localDB == nil {
				sendEvent(map[string]string{"phase": "error", "message": "local database not available"})
				return
			}

			factsIter, err := store.NewFactsIter(localDB)
			if err != nil {
				sendEvent(map[string]string{"phase": "error", "message": fmt.Sprintf("open facts iterator: %v", err)})
				return
			}
			iter := &storeFactIterAdapter{inner: factsIter}

			// Use the matched remote agent branch if found, otherwise our local agent branch name.
			agentBranch := tr.MatchedAgent
			if agentBranch == "" {
				agentBranch = gs.Branch()
			}

			cfg := git.ReplayConfig{
				Strategy:          strategy,
				AgentBranch:       agentBranch,
				DefaultBranch:     remoteBranch,
				UseExistingBranch: tr.MatchedAgent != "",
				OnProgress: func(current, total int) {
					sendEvent(map[string]any{
						"phase":   "replaying",
						"current": current,
						"total":   total,
					})
				},
			}

			replayRes, err := git.Replay(localGS, iter, remoteStore, cfg)
			if err != nil {
				sendEvent(map[string]string{"phase": "error", "message": fmt.Sprintf("replay failed: %v", err)})
				sess.mu.Lock()
				sess.State = StateError
				sess.mu.Unlock()
				return
			}

			result := applyResult{
				TotalFacts:           replayRes.TotalFacts,
				FromLocal:            replayRes.FromLocal,
				FromRemote:           replayRes.FromRemote,
				Overwrites:           replayRes.Overwrites,
				RefsResolvedFromHist: replayRes.RefsResolvedFromHist,
				DanglingRefsDropped:  replayRes.DanglingRefsDropped,
			}

			sendEvent(map[string]any{"phase": "done", "result": result})

			sess.mu.Lock()
			sess.State = StateApplied
			sess.ApplyResult = result
			sess.RemoteBranch = remoteBranch
			sess.mu.Unlock()

			log.Info().Str("repo", repo).Str("session_id", sessionID).
				Int("total", result.TotalFacts).Int("from_local", result.FromLocal).
				Int("from_remote", result.FromRemote).Int("overwrites", result.Overwrites).
				Msg("apply completed (disjoint replay)")
		} else {
			// Shared history: simple merge path.
			sendEvent(map[string]string{"phase": "merging"})

			// For v1, shared history merge is a simplified path.
			// TODO: implement full shared-history merge
			result := applyResult{}
			sendEvent(map[string]any{"phase": "done", "result": result})

			sess.mu.Lock()
			sess.State = StateApplied
			sess.ApplyResult = result
			sess.RemoteBranch = remoteBranch
			sess.mu.Unlock()

			log.Info().Str("repo", repo).Str("session_id", sessionID).
				Msg("apply completed (shared merge)")
		}
	}
}

// refsOnlyFrontmatter is used to parse only the refs field from YAML frontmatter.
type refsOnlyFrontmatter struct {
	Refs []string `yaml:"refs"`
}

// extractRefsFromFrontmatter parses YAML frontmatter and returns the refs slice.
// Returns nil if the content has no valid frontmatter.
func extractRefsFromFrontmatter(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return nil
	}
	rest := content[4:]
	closeIdx := strings.Index(rest, "\n---\n")
	if closeIdx < 0 {
		return nil
	}
	yamlBlock := rest[:closeIdx]
	var fm refsOnlyFrontmatter
	if err := yaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return nil
	}
	return fm.Refs
}

// beginSSE sets SSE headers on w and returns a sendEvent function.
// Returns nil, false if streaming is not supported.
func beginSSE(w http.ResponseWriter) (func(v any), bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return nil, false
	}
	return func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}, true
}

// collectAllBranchInfo does a single pass over refs, partitioning into non-agent
// branches, agent branches, and finding the agent branch matching localAgentBranch.
func collectAllBranchInfo(s *storegit.Storer, localAgentBranch string) (branches []string, agentBranches []string, matchedAgent string) {
	refIter, err := s.IterReferences()
	if err != nil {
		return
	}
	defer refIter.Close()

	agentSet := make(map[string]struct{})
	for {
		ref, err := refIter.Next()
		if err != nil {
			break
		}
		name := ref.Name().String()
		var short string
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			short = strings.TrimPrefix(name, "refs/heads/")
		case strings.HasPrefix(name, "refs/remotes/origin/"):
			short = strings.TrimPrefix(name, "refs/remotes/origin/")
		default:
			continue
		}
		if strings.HasPrefix(short, "agent/") {
			if _, seen := agentSet[short]; !seen {
				agentSet[short] = struct{}{}
				if short == localAgentBranch {
					matchedAgent = short
				}
			}
		} else {
			// Only count refs/heads/ as selectable branches (not remote tracking refs).
			if strings.HasPrefix(name, "refs/heads/") {
				branches = append(branches, short)
			}
		}
	}
	agentBranches = make([]string, 0, len(agentSet))
	for b := range agentSet {
		agentBranches = append(agentBranches, b)
	}
	return
}

// handleCommit handles POST /api/v1/{repo}/origin/session/{sessionID}/commit
// It finalizes the origin connection by swapping the session's remote store
// into the repo instance, saving remote config, and starting sync loops.
func handleCommit(rm *repos.Manager, sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sess, ok := sm.Get(repo, sessionID)
		if !ok {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}

		sess.mu.Lock()
		state := sess.State
		remoteStore := sess.RemoteStore
		authCfg := sess.Auth
		remoteURL := sess.URL
		remoteBranch := sess.RemoteBranch
		sess.mu.Unlock()

		if state != StateApplied {
			writeError(w, http.StatusConflict, "session must be in applied state")
			return
		}
		if remoteStore == nil {
			writeError(w, http.StatusConflict, "session has no remote store")
			return
		}

		// Use the branch chosen during apply; fall back to "main".
		if remoteBranch == "" {
			remoteBranch = "main"
		}

		ri := repos.RepoFromContext(r.Context())

		sendEvent, ok := beginSSE(w)
		if !ok {
			return
		}

		// Phase: swapping — replace the git store on the repo instance.
		sendEvent(map[string]string{"phase": "swapping"})

		// Checkpoint the temp DB's WAL so all data is in the main .db file before copying.
		if tempDB := remoteStore.Storer().DB(); tempDB != nil {
			if _, err := tempDB.ExecContext(r.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				log.Warn().Err(err).Msg("commit: WAL checkpoint failed")
			}
			tempDB.Close()
		}

		tempDBPath := filepath.Join(sess.TempDir, "clone.db")
		if err := rm.SwapStore(ri, tempDBPath); err != nil {
			sendEvent(map[string]string{"phase": "error", "message": fmt.Sprintf("swap failed: %v", err)})
			return
		}

		// Rebuild MCP handlers so they use the new database, not the closed one.
		rm.SetupMCP(ri)

		// Snapshot after swap — protect against concurrent SwapStore.
		ri.RLock()
		svc, gs, hub, startSync := ri.Svc, ri.GS, ri.Hub, ri.StartSync
		ri.RUnlock()

		// Phase: configuring — save remote config and start sync.
		sendEvent(map[string]string{"phase": "configuring"})

		if svc != nil {
			// Build the auth token for storage.
			authMethod := authCfg.Method
			authToken := assembleAuthToken(authMethod, authCfg.Token, authCfg.User, authCfg.Password)

			if err := svc.SetRemoteWithAuth("origin", remoteURL, remoteBranch, 300, 300, authMethod, authToken); err != nil {
				sendEvent(map[string]string{"phase": "error", "message": fmt.Sprintf("save remote config: %v", err)})
				return
			}
		}

		// Rebuild the index from the new git store so facts/recent/search work.
		sendEvent(map[string]any{"phase": "rebuilding", "current": 0, "total": 0})
		if svc != nil {
			if gitReader, ok := gs.(store.GitReader); ok {
				idx := svc.Index()
				progress := func(subPhase string, done, total int) {
					if done%20 == 0 || done == total {
						sendEvent(map[string]any{
							"phase":     "rebuilding",
							"sub_phase": subPhase,
							"current":   done,
							"total":     total,
						})
					}
				}
				if err := idx.Rebuild(gitReader, gs.Branch(), progress); err != nil {
					log.Warn().Err(err).Str("repo", repo).Msg("commit: index rebuild failed")
				} else {
					log.Info().Str("repo", repo).Msg("commit: index rebuilt from swapped store")
					// Set pipeline watermarks to HEAD so the first review/hypothesize
					// doesn't treat every cloned fact as dirty.
					if head, err := gs.HeadCommit(); err == nil {
						for _, tool := range []string{"review", "hypothesize"} {
							if err := idx.SetPipelineWatermark(tool, gs.Branch(), head); err != nil {
								log.Warn().Err(err).Str("repo", repo).Str("tool", tool).Msg("commit: pipeline watermark set failed")
							}
						}
					}
				}
			}
		}

		// Start sync/push loops.
		if startSync != nil {
			if err := startSync(remoteURL); err != nil {
				log.Warn().Err(err).Str("repo", repo).Msg("commit: sync activation failed")
				// Non-fatal: remote is configured, sync can be started later.
			}
		}

		sendEvent(map[string]string{"phase": "done"})

		// Broadcast status so the UI refreshes with the new HEAD.
		if hub != nil {
			if head, err := gs.HeadCommit(); err == nil {
				hub.BroadcastStatus(head)
			}
		}

		// Update session state and clean up.
		sess.mu.Lock()
		sess.State = StateCommitted
		sess.mu.Unlock()

		sm.Delete(repo, sessionID)

		log.Info().Str("repo", repo).Str("session_id", sessionID).Str("url", remoteURL).Msg("commit completed — store swapped and remote configured")
	}
}
