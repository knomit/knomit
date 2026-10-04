package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"

	"knomit/internal/config"
	knomitfact "knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// createSessionRequest is the expected JSON body for POST /origin/session.
type createSessionRequest struct {
	URL        string `json:"url"`
	AuthMethod string `json:"auth_method"`
	Token      string `json:"token"`
	User       string `json:"user"`
	Password   string `json:"password"`
}

func handleCreateSession(b hal.URLBuilder, sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")

		var req createSessionRequest
		if !decodeJSON(w, r, &req, 0) {
			return
		}
		// Before isGitURL below, and before the URL is carried on the session
		// into the origin it eventually persists.
		//
		// isGitURL does NOT reject every padded URL, which is the whole
		// trouble: url.Parse tolerates a TRAILING space, so the reported
		// defect — a URL pasted with trailing spaces — sails through and is
		// carried padded. Only a leading space, a tab or a newline make
		// url.Parse fail. So the trim is here to make the accepted form clean,
		// not merely to spare the rejected forms.
		req.URL = trimOriginURL(req.URL)

		if req.URL == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request",
				"url is required", r.URL.Path)
			return
		}
		if !isGitURL(req.URL) {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request",
				"invalid url", r.URL.Path)
			return
		}
		// A connect session ends in a store SWAP. For a subscription — which
		// owns no content of its own — that would replace the thing it follows
		// rather than reconcile with it. This route is inside RepoMiddleware,
		// so the instance is in context.
		if ri := repos.RepoFromContext(r.Context()); ri != nil && ri.Subscribed() {
			hal.WriteProblem(w, http.StatusConflict, "Subscription requires its origin",
				"a subscription cannot be re-pointed through a connect session; create a new subscription instead",
				r.URL.Path)
			return
		}
		// Local-origin policy is enforced at the clone boundary (Manager.Resolve-
		// Auth, invoked when the session is tested), so it isn't re-checked here.

		// Validate URL/auth compatibility.
		if err := validateURLAuth(req.URL, req.AuthMethod); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request",
				err.Error(), r.URL.Path)
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
			hal.WriteProblem(w, http.StatusInternalServerError, "Session creation failed",
				"failed to create session", r.URL.Path)
			return
		}

		hal.WriteHAL(w, http.StatusOK, map[string]any{
			"session_id": sess.ID,
			"_links": hal.LinkMap{
				"self": {Href: b.OriginSession(repo, sess.ID)},
			},
		})
	}
}

func handleGetSession(b hal.URLBuilder, sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sess, ok := sm.Get(repo, sessionID)
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Session not found",
				"session not found", r.URL.Path)
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

		resp["_links"] = hal.LinkMap{
			"self":       {Href: b.OriginSession(repo, sessionID)},
			"collection": {Href: b.OriginSessions(repo)},
		}
		hal.WriteHAL(w, http.StatusOK, resp)
	}
}

func handleDeleteSession(sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sm.Delete(repo, sessionID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// connectivityResult is the JSON payload sent in the "done" phase of a test connectivity SSE stream.
type connectivityResult struct {
	Branches        []string `json:"branches"`       // non-agent branches (selectable as main)
	AgentBranches   []string `json:"agent_branches"` // all agent/* branches on remote
	DefaultBranch   string   `json:"default_branch"`
	MatchedAgent    string   `json:"matched_agent,omitempty"` // agent branch matching our hostname (if any)
	History         string   `json:"history"`                 // "shared" or "disjoint"
	RemoteFactCount int      `json:"remote_fact_count"`
	LocalFactCount  int      `json:"local_fact_count"`
}

// handleTestConnectivity handles GET /api/v1/{repo}/origin/session/{sessionID}/test
func handleTestConnectivity(rm *repos.Manager, sm *SessionManager, agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sess, ok := sm.Get(repo, sessionID)
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Session not found",
				"session not found", r.URL.Path)
			return
		}

		ri := repos.RepoFromContext(r.Context())
		// Acquire pins the local store for this whole SSE flow so a concurrent
		// SwapStore/Archive drains it instead of closing the service mid-use;
		// on error localSvc stays nil, which the flow already tolerates.
		var localSvc *store.Service
		if s, release, err := ri.Acquire(); err == nil {
			localSvc = s
			defer release()
		}

		sendEvent, ok := beginSSE(w)
		if !ok {
			return
		}

		// Phase: connecting.
		sendEvent(map[string]string{"phase": "connecting"})

		// Resolve auth from session config.
		authCfg := config.RemoteAuthConfig{
			AuthMethod: sess.Auth.Method,
			Token:      sess.Auth.Token,
			User:       sess.Auth.User,
			Password:   sess.Auth.Password,
		}
		auth, err := rm.ResolveAuth(authCfg, sess.URL)
		if err != nil {
			sendEvent(map[string]string{"phase": "error", "message": fmt.Sprintf("auth resolution failed: %v", err)})
			return
		}

		// Create a temp store.Service for the clone.
		dbPath := filepath.Join(sess.TempDir, "clone.db")
		remoteSvc, err := store.Open(dbPath)
		if err != nil {
			sendEvent(map[string]string{"phase": "error", "message": fmt.Sprintf("open clone db: %v", err)})
			return
		}
		// This store is where the disjoint-history commit REPLAYS this
		// instance's facts (store.Replay), and SwapStore then installs it and
		// pushes the result as our agent branch. Those commits must be signed:
		// without a signer they went out unsigned (now store.ErrNoSigner).
		remoteSvc.SetSigner(rm.Signer())
		if ri := repos.RepoFromContext(r.Context()); ri != nil {
			remoteSvc.SetAcceptList(rm.AcceptListFor(ri.UID()))
		}

		// Phase: cloning.
		sendEvent(map[string]string{"phase": "cloning"})

		progressFn := func(msg string) {
			sendEvent(map[string]string{"phase": "cloning", "progress": msg})
		}

		cloneErr := remoteSvc.CloneFrom(sess.URL, auth, progressFn)
		if cloneErr != nil {
			remoteSvc.Close()
			msg := fmt.Sprintf("clone failed: %v", cloneErr)
			if errors.Is(cloneErr, store.ErrEmptyRemote) {
				msg = "Remote repository has no branches. Initialize it with at least one branch (e.g. `main` with an initial commit) before connecting."
			}
			log.Warn().Err(cloneErr).Str("repo", repo).Str("url", sess.URL).Msg("test connectivity: clone failed")
			sendEvent(map[string]string{"phase": "error", "message": msg})
			sess.mu.Lock()
			sess.State = StateError
			sess.mu.Unlock()
			return
		}

		// Phase: analyzing.
		sendEvent(map[string]string{"phase": "analyzing"})

		// Get default branch.
		defaultBranch, err := remoteSvc.Branches().DefaultBranch(r.Context())
		if err != nil {
			defaultBranch = agentBranch
		}

		// One local copy per knowledge base. Check here, at the FIRST step of
		// the wizard, so a doomed connect fails before any preview or replay
		// work — and long before the swap, which cannot be undone.
		//
		// The verdict is delivered as an SSE "error" phase, not a 409: this
		// handler streams, so the 200 and the text/event-stream content type
		// were committed by the "connecting" event above, before the clone
		// this check needs even existed. Every other post-clone failure here
		// (auth, clone) reports the same way. The session is moved to
		// StateError so /apply and /commit refuse it outright.
		if rootCommit, rcErr := remoteSvc.RootCommit(r.Context(), defaultBranch); rcErr == nil && rootCommit != "" {
			holder, hErr := repos.HeldByAnotherActiveRepo(rm, ri.UID(), rootCommit)
			if hErr != nil || holder != "" {
				var msg string
				if hErr != nil {
					msg = fmt.Sprintf("registry unavailable: %v", hErr)
				} else {
					msg = fmt.Sprintf("this remote holds the same knowledge base as the repo %q; "+
						"two local copies would clobber each other's agent branch on push", holder)
					log.Warn().Str("repo", repo).Str("holder", holder).Str("url", sess.URL).
						Msg("test connectivity: refused — remote already registered to another repo")
				}
				remoteSvc.Close()
				sendEvent(map[string]string{"phase": "error", "message": msg})
				sess.mu.Lock()
				sess.State = StateError
				sess.mu.Unlock()
				return
			}
		}

		// Collect all branch info in a single pass over refs.
		branches, agentBranches, matchedAgent := remoteSvc.Branches().BranchInfo(agentBranch)

		// Check shared history.
		history := "disjoint"
		if localSvc != nil {
			shared, err := localSvc.HasSharedHistory(r.Context(), agentBranch, remoteSvc, defaultBranch)
			if err == nil && shared {
				history = "shared"
			}
		}

		// Count remote facts (files in the cloned store).
		remoteFiles, err := remoteSvc.Facts().ListAll(r.Context(), defaultBranch)
		remoteFactCount := 0
		if err == nil {
			remoteFactCount = len(remoteFiles)
		}

		// Count local facts.
		localFactCount := 0
		if localSvc != nil {
			localFiles, err := localSvc.Facts().ListAll(r.Context(), agentBranch)
			if err == nil {
				localFactCount = len(localFiles)
			}
		}

		result := connectivityResult{
			Branches:        branches,
			AgentBranches:   agentBranches,
			DefaultBranch:   defaultBranch,
			MatchedAgent:    matchedAgent,
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
		sess.RemoteStore = remoteSvc
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
func handlePreview(sm *SessionManager, agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sess, ok := sm.Get(repo, sessionID)
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Session not found",
				"session not found", r.URL.Path)
			return
		}

		sess.mu.Lock()
		state := sess.State
		remoteStore := sess.RemoteStore
		testResult, _ := sess.TestResult.(connectivityResult)
		sess.mu.Unlock()

		if state != StateTested && state != StatePreviewed && state != StateApplied {
			hal.WriteProblem(w, http.StatusConflict, "Session state conflict",
				"session must be in tested state or later", r.URL.Path)
			return
		}
		if remoteStore == nil {
			hal.WriteProblem(w, http.StatusConflict, "Session state conflict",
				"session has no remote store (run test first)", r.URL.Path)
			return
		}

		// Use the matched remote agent branch if available (from test step).
		// If no match, fall back to the remote's default branch (e.g. "main"),
		// which contains the merged state of all facts. Last resort: local branch name.
		remoteAgentBranch := testResult.MatchedAgent
		if remoteAgentBranch == "" {
			remoteAgentBranch = testResult.DefaultBranch
		}
		if remoteAgentBranch == "" {
			remoteAgentBranch = agentBranch
		}

		ri := repos.RepoFromContext(r.Context())
		// Acquire pins the local store for this whole SSE flow (see
		// handleTestConnectivity); nil svc is tolerated below.
		var svc *store.Service
		if s, release, err := ri.Acquire(); err == nil {
			svc = s
			defer release()
		}

		sendEvent, ok := beginSSE(w)
		if !ok {
			return
		}

		sendEvent(map[string]string{"phase": "comparing"})

		// Build local path set via FactsIter.
		localPaths := make(map[string]struct{})
		if svc != nil {
			iter, err := svc.FactQuery().FactsIter(r.Context(), agentBranch)
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
		remoteFiles, err := remoteStore.Facts().ListAll(r.Context(), remoteAgentBranch)
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
		// Resolved once here rather than per fact — ri.ID() caches, but the
		// workers below run concurrently and this keeps the value obviously
		// constant across them.
		localRepoID := knomitfact.ID12(ri.ID())
		const workers = 8
		jobs := make(chan string, len(localPaths))
		results := make(chan int, len(localPaths))
		var readMu sync.Mutex

		for i := 0; i < workers; i++ {
			go func() {
				for p := range jobs {
					readMu.Lock()
					readResult, err := svc.Facts().ReadFact(r.Context(), agentBranch, p, nil)
					readMu.Unlock()
					if err != nil {
						results <- 0
						continue
					}
					dead := 0
					for _, ref := range extractRefsFromFrontmatter(readResult.Content) {
						// Only a ref naming a fact in THIS repo can be dead.
						// The http(s)-only skip this replaced counted every
						// src:// citation, file:/// ref and cross-repo kb://
						// pointer as dead, inflating the count shown to the
						// user. localRepoID must be the real id: refs are
						// stored canonical, so classifying with "" would read
						// every local ref as foreign and report zero dead refs
						// forever.
						c := knomitfact.ClassifyRef(ref, localRepoID)
						if c.Kind != knomitfact.RefLocalFact {
							continue
						}
						if _, alive := localPaths[c.Path]; !alive {
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
func handleApply(sm *SessionManager, agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sess, ok := sm.Get(repo, sessionID)
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Session not found",
				"session not found", r.URL.Path)
			return
		}

		sess.mu.Lock()
		state := sess.State
		remoteStore := sess.RemoteStore
		testResult := sess.TestResult
		sess.mu.Unlock()

		if state != StateTested && state != StatePreviewed && state != StateApplied {
			hal.WriteProblem(w, http.StatusConflict, "Session state conflict",
				"session must be tested before applying", r.URL.Path)
			return
		}
		if remoteStore == nil {
			hal.WriteProblem(w, http.StatusConflict, "Session state conflict",
				"session has no remote store (run test first)", r.URL.Path)
			return
		}

		var req applyRequest
		if !decodeJSON(w, r, &req, 0) {
			return
		}

		strategy := store.ConflictStrategy(req.ConflictStrategy)
		if strategy != store.StrategyLocalWins && strategy != store.StrategyRemoteWins {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request",
				"conflict_strategy must be local_wins or remote_wins", r.URL.Path)
			return
		}

		// Extract test result to get history type and default branch.
		tr, ok := testResult.(connectivityResult)
		if !ok {
			hal.WriteProblem(w, http.StatusConflict, "Session state conflict",
				"session test result missing or invalid", r.URL.Path)
			return
		}

		// Resolve the remote branch to track: explicit request > test result default.
		remoteBranch := tr.DefaultBranch
		if req.Branch != "" {
			remoteBranch = req.Branch
		}

		ri := repos.RepoFromContext(r.Context())
		// Acquire pins the local store for the whole replay (see
		// handleTestConnectivity); nil svc is rejected below before use.
		var svc *store.Service
		if s, release, err := ri.Acquire(); err == nil {
			svc = s
			defer release()
		}

		sendEvent, ok := beginSSE(w)
		if !ok {
			return
		}

		if tr.History == "disjoint" {
			sendEvent(map[string]string{"phase": "replaying"})

			if svc == nil {
				sendEvent(map[string]string{"phase": "error", "message": "local database not available"})
				return
			}

			factsIter, err := svc.FactQuery().FactsIter(r.Context(), agentBranch)
			if err != nil {
				sendEvent(map[string]string{"phase": "error", "message": fmt.Sprintf("open facts iterator: %v", err)})
				return
			}

			// Use the matched remote agent branch if found, otherwise our local agent branch name.
			replayAgentBranch := tr.MatchedAgent
			if replayAgentBranch == "" {
				replayAgentBranch = agentBranch
			}

			cfg := store.ReplayConfig{
				Strategy:          strategy,
				AgentBranch:       replayAgentBranch,
				DefaultBranch:     remoteBranch,
				UseExistingBranch: tr.MatchedAgent != "",
				// The clone is fully reindexed by IndexManager().Rebuild at commit
				// time (handleCommit), so skip the per-fact index sync here — it
				// would otherwise full-rebuild on the first write then incrementally
				// re-sync for every remaining fact, all of it thrown away by Rebuild.
				SkipIndexSync: true,
				OnProgress: func(current, total int) {
					sendEvent(map[string]any{
						"phase":   "replaying",
						"current": current,
						"total":   total,
					})
				},
			}

			replayRes, err := store.Replay(r.Context(), svc, agentBranch, factsIter, remoteStore, cfg)
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
			sess.AppliedBranch = replayAgentBranch
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

			// For shared history, the cloned store's HEAD is the remote's agent branch.
			// Use the matched agent branch, or the first known remote agent branch,
			// or fall back to localAgentBranch if none found.
			sharedAppliedBranch := tr.MatchedAgent
			if sharedAppliedBranch == "" && len(tr.AgentBranches) > 0 {
				sharedAppliedBranch = tr.AgentBranches[0]
			}
			if sharedAppliedBranch == "" {
				sharedAppliedBranch = agentBranch
			}

			sess.mu.Lock()
			sess.State = StateApplied
			sess.ApplyResult = result
			sess.AppliedBranch = sharedAppliedBranch
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
//
// Backed by sseStream, so all four flows that use it — test, preview, apply
// and commit — get the bounded, error-checked write described in sse.go.
//
// sendEvent stays `func(v any)` with no result, deliberately. These handlers
// thread it through long procedures (commitSharedHistory and friends) whose
// work is not abortable half-way: a clone that has swapped a store must finish
// swapping it whether or not a browser is still listening. The `dead` latch in
// sseStream is what makes ignoring the result safe — after the first failure
// every later send is a cheap no-op rather than another blocked write, so a
// vanished client costs the handler nothing and cannot wedge it.
//
// That is the ONE place in this package where a write result is not turned
// into a return; everywhere else, `if !s.Write(...) { return }`.
func beginSSE(w http.ResponseWriter) (func(v any), bool) {
	stream, ok := startSSE(w)
	if !ok {
		return nil, false
	}
	return func(v any) {
		data, _ := json.Marshal(v)
		stream.Write("data: %s\n\n", data)
	}, true
}

// handleListSessions serves GET /repos/{repo}/origin-sessions.
// Returns a JSON array of active sessions for the given repo.
func handleListSessions(b hal.URLBuilder, sm *SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessions := sm.ListByRepo(repo)
		type sessionSummary struct {
			SessionID string `json:"session_id"`
			State     string `json:"state"`
			URL       string `json:"url"`
		}
		out := make([]sessionSummary, 0, len(sessions))
		for _, s := range sessions {
			s.mu.Lock()
			out = append(out, sessionSummary{
				SessionID: s.ID,
				State:     string(s.State),
				URL:       s.URL,
			})
			s.mu.Unlock()
		}
		hal.WriteHAL(w, http.StatusOK, hal.CollectionView[sessionSummary]{
			Count:    len(out),
			Links:    hal.LinkMap{"self": {Href: b.OriginSessions(repo)}},
			Embedded: map[string][]sessionSummary{"sessions": out},
		})
	}
}

// commitRequest is the optional JSON body of POST .../commit.
type commitRequest struct {
	// CancelIndexing asks the commit to cancel the repo's index job first
	// ("cancel indexing and continue"). Without it, a commit while the repo is
	// indexing is refused with 409 and a cancel-index link.
	CancelIndexing bool `json:"cancel_indexing"`
}

// handleCommit handles POST /api/v1/{repo}/origin/session/{sessionID}/commit
//
// It finalizes the origin connection as ONE lifecycle event on the repo's
// machine, streaming the machine's transitions as progress phases:
//
//   - shared history: AttachOrigin (phases configuring, done). The local store
//     already shares commits with the remote; the sync loop's own merge
//     primitives reconcile them, and swapping would discard local-only facts.
//   - disjoint history: SwapStore with the session's origin (phases swapping,
//     configuring, indexing with progress, done). The machine exits every
//     stage, installs the clone over the repo's database and persists the
//     origin as one apply, then walks the repo up again — the heal of the new
//     store is the post-swap index. A failed install restores the previous
//     store and still walks it up; the reply says so.
//
// Both are exclusive with indexing: refused with 409 (and a cancel-index link)
// before anything is touched, unless the body asks to cancel indexing first —
// then the stream opens with phase cancelling-index. A refusal that arrives
// before the machine changed anything (a different knowledge base, a remote
// already held by another repo) is still a plain HTTP problem: the stream
// opens only once the event is under way.
func (s *Server) handleCommit(rm *repos.Manager, sm *SessionManager, agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")

		sess, ok := sm.Get(repo, sessionID)
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Session not found",
				"session not found", r.URL.Path)
			return
		}

		var body commitRequest
		if !decodeOptionalJSON(w, r, &body, 0) {
			return
		}

		sess.mu.Lock()
		state := sess.State
		remoteStore := sess.RemoteStore
		authCfg := sess.Auth
		remoteURL := sess.URL
		appliedBranch := sess.AppliedBranch
		appliedRemoteBranch := sess.RemoteBranch
		testResult, _ := sess.TestResult.(connectivityResult)
		sess.mu.Unlock()

		if state != StateApplied {
			hal.WriteProblem(w, http.StatusConflict, "Session state conflict",
				"session must be in applied state", r.URL.Path)
			return
		}
		if remoteStore == nil {
			hal.WriteProblem(w, http.StatusConflict, "Session state conflict",
				"session has no remote store", r.URL.Path)
			return
		}

		ri := repos.RepoFromContext(r.Context())
		b := hal.URLBuilder{Base: APIBase}

		// Exclusive with indexing, checked before the session's clone is
		// closed, so a refused commit can be retried with cancel_indexing.
		if ri.Status().IndexRunning() && !body.CancelIndexing {
			writeLifecycleProblem(w, r, b, repo, repos.ErrIndexing)
			return
		}

		// The branch the user chose at /apply time, else the remote's own
		// default (the test clone's HEAD). With neither there is no branch to
		// record, and the attach refuses rather than inventing one.
		upstreamMain := appliedRemoteBranch
		if upstreamMain == "" {
			upstreamMain = testResult.DefaultBranch
		}
		origin := repos.OriginSpec{
			URL:        remoteURL,
			Branch:     upstreamMain,
			AuthMethod: authCfg.Method,
			AuthToken:  assembleAuthToken(authCfg.Method, authCfg.Token, authCfg.User, authCfg.Password),
		}

		var e repos.MachineEvent
		attach := true // false: the shared-history commit has no branch to record
		if testResult.History == "shared" {
			// Shared history means the remote holds the SAME knowledge base
			// this repo already does, so the one-copy-per-knowledge-base rule
			// bites here too. /test refuses this first; re-check while
			// refusing is still free.
			if rootCommit := ri.ID(); rootCommit != "" {
				holder, hErr := repos.HeldByAnotherActiveRepo(rm, ri.UID(), rootCommit)
				if hErr != nil {
					hal.WriteProblem(w, http.StatusServiceUnavailable, "Registry unavailable", hErr.Error(), r.URL.Path)
					return
				}
				if holder != "" {
					hal.WriteProblem(w, http.StatusConflict, "Knowledge base already local", fmt.Sprintf(
						"this remote holds the same knowledge base as the repo %q; connect aborted before any change was made", holder), r.URL.Path)
					return
				}
			}
			// With neither a chosen branch nor the remote's default, the session
			// established no consensus branch: record NO origin (the warning
			// says why) rather than let the attach fall back to one.
			if upstreamMain == "" {
				attach = false
			}
			e = repos.AttachOrigin(origin)
		} else {
			// A swap installs the remote's store; with no consensus branch
			// there is no origin to record alongside it, and installing the
			// store without its origin would leave it under whatever origin
			// control.db held before. Refused while refusing is still free.
			if upstreamMain == "" {
				hal.WriteProblem(w, http.StatusBadRequest, "No consensus branch",
					"save remote config: "+store.ErrNoConsensusBranch.Error()+"; choose the remote branch to follow and commit again",
					r.URL.Path)
				return
			}
			swapBranch := appliedBranch
			if swapBranch == "" {
				swapBranch = agentBranch
			}
			e = repos.SwapStore(repos.SwapSpec{
				TempDB:         filepath.Join(sess.TempDir, "clone.db"),
				Origin:         origin,
				IdentityBranch: swapBranch,
			})
		}

		// The clone has served its purpose: checkpoint its WAL so the swap
		// copies a complete file, close it, and detach it from the session so
		// a retry cannot reuse a closed handle (re-entry then hits the "no
		// remote store" guard above).
		if err := remoteStore.Checkpoint(); err != nil {
			log.Warn().Err(err).Msg("commit: WAL checkpoint failed")
		}
		remoteStore.Close()
		sess.mu.Lock()
		sess.RemoteStore = nil
		sess.mu.Unlock()

		stream := &lazyStream{w: w}
		if body.CancelIndexing {
			if !stream.event(map[string]string{"phase": "cancelling-index"}) {
				return
			}
			if _, err := rm.Send(r.Context(), ri, repos.CancelIndex()); err != nil {
				stream.event(map[string]string{"phase": "error", "message": err.Error()})
				return
			}
		}
		if attach {
			if !streamLifecycle(w, r, b, rm, ri, repo, stream, e) {
				return
			}
		} else {
			stream.event(map[string]string{"phase": "configuring"})
			stream.event(map[string]any{"phase": "done", "index_state": ri.Status().Index.State,
				"warning": "save remote config: " + store.ErrNoConsensusBranch.Error()})
		}

		sess.mu.Lock()
		sess.State = StateCommitted
		sess.mu.Unlock()
		sm.Delete(repo, sessionID)
		log.Info().Str("repo", repo).Str("session_id", sessionID).Str("url", remoteURL).
			Str("history", testResult.History).Msg("commit completed")
	}
}

// lazyStream is an SSE stream that opens on its first event, so a handler can
// still answer with a plain HTTP problem until something has happened.
type lazyStream struct {
	w    http.ResponseWriter
	send func(any)
}

func (s *lazyStream) opened() bool { return s.send != nil }

// event opens the stream if needed and writes v; false when the response
// cannot stream.
func (s *lazyStream) event(v any) bool {
	if s.send == nil {
		send, ok := beginSSE(s.w)
		if !ok {
			return false
		}
		s.send = send
	}
	s.send(v)
	return true
}

// streamLifecycle sends e to ri's machine and streams its transitions as
// phases until the repo's index has left "indexing": swapping (the machine is
// rewinding to Populate), configuring (Open, Identify), indexing with
// current/total, then done (carrying index_state, and a warning carrying the
// index's reason when it ended in error) or error with the event's own failure. A refusal that
// arrives while the stream is still closed is answered as an HTTP problem.
// It reports whether the event succeeded.
func streamLifecycle(w http.ResponseWriter, r *http.Request, b hal.URLBuilder, rm *repos.Manager,
	ri *repos.RepoInstance, repo string, stream *lazyStream, e repos.MachineEvent) bool {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	watch := ri.Watch(ctx)

	type result struct {
		rep repos.Reply
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := rm.Send(ctx, ri, e)
		done <- result{rep, err}
	}()

	phase := ""
	emitPhase := func(st repos.Status) {
		switch st.Stage {
		case "populate":
			if phase == "" {
				phase = "swapping"
				stream.event(map[string]string{"phase": "swapping"})
			}
		case "open", "identify":
			if phase != "configuring" {
				if phase == "" {
					stream.event(map[string]string{"phase": "swapping"})
				}
				phase = "configuring"
				stream.event(map[string]string{"phase": "configuring"})
			}
		}
	}

	var sent bool
	var sendErr error
	for !sent {
		select {
		case t, ok := <-watch:
			if !ok {
				watch = nil
				continue
			}
			emitPhase(t.Status)
		case res := <-done:
			sent, sendErr = true, res.err
		}
	}
	if sendErr != nil {
		// A refusal changed nothing: still a plain HTTP problem while the
		// stream is closed. Anything else failed after the machine had begun
		// (a failed install, an unopenable store) and is a phase.
		if !stream.opened() && isCommitRefusal(sendErr) {
			writeCommitProblem(w, r, b, repo, sendErr)
			return false
		}
		stream.event(map[string]string{"phase": "error", "message": sendErr.Error()})
		return false
	}
	if phase == "" {
		stream.event(map[string]string{"phase": "configuring"})
	}
	// The event is applied; the index job of the (possibly new) store runs in
	// the background. Narrate it until it leaves "indexing".
	st := ri.Status()
	for st.Index.State == repos.IndexStateIndexing && watch != nil {
		stream.event(map[string]any{"phase": "indexing", "current": st.Index.Done, "total": st.Index.Total})
		t, ok := <-watch
		if !ok {
			break
		}
		st = t.Status
	}
	doneEv := map[string]any{"phase": "done", "index_state": st.Index.State}
	if st.Index.Reason != "" {
		doneEv["warning"] = "index: " + st.Index.Reason
	}
	stream.event(doneEv)
	return true
}

// isCommitRefusal reports whether err is a refusal that changed nothing: the
// machine's own checks, or an event guard's verdict.
func isCommitRefusal(err error) bool {
	return isLifecycleRefusal(err) ||
		errors.Is(err, repos.ErrOriginOntologyConflict) ||
		errors.Is(err, repos.ErrKnowledgeBaseAlreadyLocal) ||
		errors.Is(err, repos.ErrOriginUnreachable) ||
		errors.Is(err, repos.ErrLocalOriginDenied) ||
		errors.Is(err, repos.ErrRegistryUnavailable)
}

// writeCommitProblem maps a commit refused before anything changed.
func writeCommitProblem(w http.ResponseWriter, r *http.Request, b hal.URLBuilder, repo string, err error) {
	switch {
	case isLifecycleRefusal(err):
		writeLifecycleProblem(w, r, b, repo, err)
	case errors.Is(err, repos.ErrOriginOntologyConflict):
		hal.WriteProblem(w, http.StatusConflict, "Different knowledge base", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrKnowledgeBaseAlreadyLocal):
		hal.WriteProblem(w, http.StatusConflict, "Knowledge base already local", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrOriginUnreachable):
		hal.WriteProblem(w, http.StatusBadGateway, "Origin not attached", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrLocalOriginDenied):
		hal.WriteProblem(w, http.StatusBadRequest, "Origin not allowed", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrRegistryUnavailable):
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Registry unavailable", err.Error(), r.URL.Path)
	default:
		hal.WriteProblem(w, http.StatusInternalServerError, "Commit failed", err.Error(), r.URL.Path)
	}
}
