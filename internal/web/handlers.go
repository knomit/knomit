// Package web provides the knomit HTTP server: REST API, SSE event stream,
// Smart HTTP git remote, and embedded SPA frontend.
//
// Architecture:
//
//   - All handlers accept narrow interfaces (GitStore, SearchIndex) rather
//     than concrete types, making them testable with hand-rolled mocks.
//   - Long-running operations (synthesis, git sync) execute asynchronously
//     via TaskHub; clients observe progress through the SSE /api/v1/events
//     endpoint.
//   - The frontend is an embedded SPA served with client-side routing
//     fallback (embed.go / embed_noembed.go).
//
// Files in this package:
//
//   - server.go          — NewRouter: chi mux wiring, dependency interfaces.
//   - handlers.go        — Read-only query handlers (browse, fact, search,
//     history, stats, status) and JSON helpers.
//   - handlers_task.go   — Async task handlers (synthesize, sync) and helpers.
//   - handlers_stream.go — SSE endpoint (handleEvents).
//   - taskhub.go         — TaskHub: per-op single-flight, pub/sub broadcasting.
//   - gitremote.go       — Smart HTTP git remote (upload-pack, receive-pack).
//   - openapi_handler.go — Embedded OpenAPI spec and Swagger UI.
//   - embed.go           — Embedded SPA assets (build tag: !noembed).
//   - embed_noembed.go   — 404 stub when compiled without SPA (build tag: noembed).
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"knomit/internal/git"
	"knomit/internal/mcp"
	"knomit/internal/repos"
	"knomit/internal/store"

	"github.com/rs/zerolog/log"
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

// handleBrowse handles GET /api/v1/{repo}/browse?path=<path>.
// When the path parameter is empty, it defaults to ontologyRoot — the
// configured knowledge-base root — so the UI lands on a meaningful
// starting directory rather than the repository top level.
func handleBrowse(ontologyRoot, agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		var gs repos.GitStore
		var idx repos.SearchIndex
		ri.WithRead(func(d repos.StoreDeps) {
			gs = d.GS
			idx = d.Idx
		})
		path := r.URL.Query().Get("path")
		if path == "" {
			path = ontologyRoot
		}

		entries, err := gs.ListDir(agentBranch, path)
		if err != nil {
			// Empty repo or missing directory — return empty list, not an error.
			log.Debug().Err(err).Str("path", path).Msg("browse: directory not found, returning empty")
			writeJSON(w, http.StatusOK, map[string]any{
				"path":     path,
				"children": []struct{}{},
			})
			return
		}

		type child struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
			Type  string `json:"type,omitempty"`
			Title string `json:"title,omitempty"`
		}

		// Batch-fetch epistemic types and titles for fact files from the index.
		typeByPath := map[string]string{}
		titleByPath := map[string]string{}
		if idx != nil {
			var factPaths []string
			for _, e := range entries {
				if !e.IsDir {
					factPaths = append(factPaths, path+"/"+e.Name)
				}
			}
			if len(factPaths) > 0 {
				for _, fp := range factPaths {
					if fb, err := idx.GetByPath(agentBranch, fp); err == nil && fb != nil {
						typeByPath[fp] = fb.Type
						titleByPath[fp] = fb.Title
					}
				}
			}
		}

		children := make([]child, 0, len(entries))
		for _, e := range entries {
			c := child{Name: e.Name, IsDir: e.IsDir}
			if !e.IsDir {
				c.Type = typeByPath[path+"/"+e.Name]
				c.Title = titleByPath[path+"/"+e.Name]
			}
			children = append(children, c)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"path":     path,
			"children": children,
		})
	}
}

// handleFact handles GET /api/v1/{repo}/fact?path=<path>&commit=<hash>
func handleFact(agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		var gs repos.GitStore
		var svc *store.Service
		ri.WithRead(func(d repos.StoreDeps) {
			gs = d.GS
			svc = d.Svc
		})
		path := r.URL.Query().Get("path")
		if path == "" {
			writeError(w, http.StatusBadRequest, "path query parameter is required")
			return
		}

		commitHash := r.URL.Query().Get("commit")

		var content string
		var fromCommit string
		var err error
		if commitHash != "" {
			content, err = gs.ReadFileAtCommit(agentBranch, path, commitHash)
			if err != nil {
				// File may have been deleted in this commit (e.g. retract).
				// Fall back to the last commit where the file existed.
				content, fromCommit, err = gs.ReadFileLastCommit(agentBranch, path, commitHash)
			} else {
				fromCommit = commitHash
			}
			if err != nil && svc != nil {
				if lastHash, ok := svc.Index().LastCommitForPath(agentBranch, path); ok {
					content, err = gs.ReadFileAtCommit(agentBranch, path, lastHash)
					if err == nil {
						fromCommit = lastHash
					}
				}
			}
		} else {
			content, err = gs.ReadFile(agentBranch, path)
		}
		if err != nil {
			log.Debug().Err(err).Str("path", path).Msg("fact not found")
			writeError(w, http.StatusNotFound, fmt.Sprintf("fact not found: %v", err))
			return
		}

		fact, err := mcp.ParseFact(path, content)
		if err != nil {
			// File could not be parsed as a fact — return raw content with parse error.
			writeJSON(w, http.StatusOK, map[string]any{
				"path":        path,
				"title":       path,
				"body":        content,
				"parse_error": err.Error(),
			})
			return
		}

		// Lowercase local refs so they match stored fact paths (always lowercase).
		// A ref is local if it has no scheme (no "://" prefix).
		for i, ref := range fact.Refs {
			if !strings.Contains(ref, "://") {
				fact.Refs[i] = strings.ToLower(ref)
			}
		}

		if fromCommit != "" {
			writeJSON(w, http.StatusOK, map[string]any{
				"path":        fact.Path(),
				"title":       fact.Title,
				"type":        fact.Type,
				"body":        fact.Body,
				"domain":      fact.Domain,
				"confidence":  fact.Confidence,
				"sources":     fact.Sources,
				"entities":    fact.Entities,
				"refs":        fact.Refs,
				"from_commit": fromCommit,
				"commit_hash": fromCommit,
			})
			return
		}

		// Browsing mode: enrich with commit hash and date from the store index.
		if commitHash == "" && svc != nil {
			if rec, lerr := svc.Index().GetByPath(agentBranch, path); lerr == nil && rec != nil && rec.CommitHash != "" {
				resp := map[string]any{
					"path":        fact.Path(),
					"title":       fact.Title,
					"type":        fact.Type,
					"body":        fact.Body,
					"domain":      fact.Domain,
					"confidence":  fact.Confidence,
					"sources":     fact.Sources,
					"entities":    fact.Entities,
					"refs":        fact.Refs,
					"commit_hash": rec.CommitHash,
				}
				if ts, ok := svc.Index().CommitTimestamp(rec.CommitHash); ok {
					resp["commit_date"] = time.Unix(ts, 0).UTC().Format(time.RFC3339)
				}
				writeJSON(w, http.StatusOK, resp)
				return
			}
		}
		writeJSON(w, http.StatusOK, fact)
	}
}

// handleFactWrite handles PUT /api/v1/{repo}/fact — writes raw fact file content.
// Request body: JSON {"path": "...", "content": "..."}
// Response: the re-parsed fact JSON (or parse error if content is still invalid).
func handleFactWrite(agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		var gs repos.GitStore
		ri.WithRead(func(d repos.StoreDeps) { gs = d.GS })

		var req struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Path == "" {
			writeError(w, http.StatusBadRequest, "path is required")
			return
		}

		msg := "edit: update " + req.Path + " via UI"
		if _, _, err := gs.WriteFile(agentBranch, req.Path, req.Content, msg, "update"); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("write failed: %v", err))
			return
		}

		fact, err := mcp.ParseFact(req.Path, req.Content)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"path":        req.Path,
				"title":       req.Path,
				"body":        req.Content,
				"parse_error": err.Error(),
			})
			return
		}

		writeJSON(w, http.StatusOK, fact)
	}
}

// handleFactRetract handles DELETE /api/v1/{repo}/fact?path=<path>.
// Deletes the fact file and commits with operation "retract".
func handleFactRetract(agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		var gs repos.GitStore
		ri.WithRead(func(d repos.StoreDeps) { gs = d.GS })

		path := r.URL.Query().Get("path")
		if path == "" {
			writeError(w, http.StatusBadRequest, "path query parameter is required")
			return
		}

		msg := "manual-review: retract " + path
		commitHash, err := gs.DeleteFile(agentBranch, path, msg, "retract")
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("retract failed: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"commit": commitHash})
	}
}

// handleSearch handles GET /api/v1/{repo}/search?q=<query>&entities=<e1,e2>&domain=<d1,d2>&path=<p>&min_confidence=<f>&limit=<n>.
// The entities and domain filters are AND-combined (all specified values
// must match). Each accepts a comma-separated list of terms.
// Additional filters: type=<t1,t2>, exclude_type=<t1>, ep=<op1,op2>
func handleSearch() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		branch := ri.Branch()
		var idx repos.SearchIndex
		ri.WithRead(func(d repos.StoreDeps) { idx = d.Idx })
		if idx == nil {
			writeError(w, http.StatusBadRequest, "search index not available")
			return
		}

		q := r.URL.Query()
		text := q.Get("q")
		entitiesStr := q.Get("entities")
		domainStr := q.Get("domain")
		path := q.Get("path")
		minConfidenceStr := q.Get("min_confidence")
		minSimilarityStr := q.Get("min_similarity")
		limitStr := q.Get("limit")
		graphHopsStr := q.Get("graph_hops")
		typeStr := q.Get("type")
		excludeTypeStr := q.Get("exclude_type")
		epStr := q.Get("ep")

		var entities []string
		if entitiesStr != "" {
			for _, e := range strings.Split(entitiesStr, ",") {
				e = strings.TrimSpace(e)
				if e != "" {
					entities = append(entities, e)
				}
			}
		}

		var domain []string
		if domainStr != "" {
			for _, d := range strings.Split(domainStr, ",") {
				d = strings.TrimSpace(d)
				if d != "" {
					domain = append(domain, d)
				}
			}
		}

		var minConfidence float64
		if minConfidenceStr != "" {
			v, err := strconv.ParseFloat(minConfidenceStr, 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid min_confidence value")
				return
			}
			minConfidence = v
		}

		var minSimilarity float64
		if minSimilarityStr != "" {
			v, err := strconv.ParseFloat(minSimilarityStr, 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid min_similarity value")
				return
			}
			minSimilarity = v
		}

		limit := 50
		if limitStr != "" {
			v, err := strconv.Atoi(limitStr)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid limit value")
				return
			}
			limit = v
		}
		if limit > 500 {
			limit = 500
		}

		var graphHops int
		if graphHopsStr != "" {
			v, err := strconv.Atoi(graphHopsStr)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid graph_hops value")
				return
			}
			graphHops = v
		} else {
			graphHops = 1 // default
		}

		var includeTypes []string
		if typeStr != "" {
			for _, t := range strings.Split(typeStr, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					includeTypes = append(includeTypes, t)
				}
			}
		}

		var excludeTypes []string
		if excludeTypeStr != "" {
			for _, t := range strings.Split(excludeTypeStr, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					excludeTypes = append(excludeTypes, t)
				}
			}
		}

		var episodeOps []string
		if epStr != "" {
			for _, op := range strings.Split(epStr, ",") {
				op = strings.TrimSpace(op)
				if op != "" {
					episodeOps = append(episodeOps, op)
				}
			}
		}

		log.Debug().Str("q", text).Strs("entities", entities).Strs("domain", domain).Int("limit", limit).Msg("search")

		results, err := idx.Search(branch, store.SearchQuery{
			Text:          text,
			Entities:      entities,
			Domain:        domain,
			Path:          path,
			MinConfidence: minConfidence,
			MinSimilarity: minSimilarity,
			Limit:         limit,
			GraphHops:     graphHops,
			IncludeTypes:  includeTypes,
			ExcludeTypes:  excludeTypes,
			EpisodeOps:    episodeOps,
		})
		if err != nil {
			log.Debug().Err(err).Msg("search failed")
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("search error: %v", err))
			return
		}

		if results == nil {
			results = []store.SearchResult{}
		}

		log.Debug().Int("results", len(results)).Msg("search done")
		writeJSON(w, http.StatusOK, map[string]any{
			"results": results,
		})
	}
}

// handleHistoryPaginated handles GET /api/v1/{repo}/history?path=<path>&limit=50&after=<cursor>
func handleHistoryPaginated(agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		var gs repos.GitStore
		ri.WithRead(func(d repos.StoreDeps) { gs = d.GS })
		path := r.URL.Query().Get("path")

		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > 500 {
			limit = 500
		}

		after := r.URL.Query().Get("after")
		from := r.URL.Query().Get("from")
		before := r.URL.Query().Get("before")

		entries, next, prev, err := gs.LogPaginated(agentBranch, path, limit, after, from, before)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("log error: %v", err))
			return
		}
		if entries == nil {
			entries = []git.LogEntryWithTags{}
		}

		resp := map[string]any{"entries": entries}
		if next != "" {
			resp["next"] = next
		}
		if prev != "" {
			resp["prev"] = prev
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// handleCommitDetail handles GET /api/v1/{repo}/commit?hash=<hash>
func handleCommitDetail(agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		var gs repos.GitStore
		var idx repos.SearchIndex
		ri.WithRead(func(d repos.StoreDeps) {
			gs = d.GS
			idx = d.Idx
		})
		hash := r.URL.Query().Get("hash")
		if hash == "" {
			writeError(w, http.StatusBadRequest, "hash query parameter is required")
			return
		}

		detail, err := gs.CommitDetail(hash)
		if err != nil {
			writeError(w, http.StatusNotFound, fmt.Sprintf("commit not found: %v", err))
			return
		}

		// Enrich files with titles from the index.
		type fileWithTitle struct {
			Path   string `json:"path"`
			Action string `json:"action"`
			Title  string `json:"title,omitempty"`
		}
		files := make([]fileWithTitle, len(detail.Files))
		for i, f := range detail.Files {
			files[i] = fileWithTitle{Path: f.Path, Action: f.Action}
			// Try index first (fast, works for facts still in the current state).
			if idx != nil {
				if fb, err := idx.GetByPath(agentBranch, f.Path); err == nil && fb != nil {
					files[i].Title = fb.Title
					continue
				}
			}
			// Fallback: read the file as it was at this commit and parse the title.
			// Covers retracted facts, deleted files, and anything not in the current index.
			if content, err := gs.ReadFileAtCommit(agentBranch, f.Path, hash); err == nil && content != "" {
				if parsed, perr := mcp.ParseFact(f.Path, content); perr == nil {
					files[i].Title = parsed.Title
					continue
				}
			}
			// Last resort for deleted files: find the last commit where the file existed.
			if content, _, err := gs.ReadFileLastCommit(agentBranch, f.Path, hash); err == nil && content != "" {
				if parsed, perr := mcp.ParseFact(f.Path, content); perr == nil {
					files[i].Title = parsed.Title
				}
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"commit":    detail.Commit,
			"date":      detail.Date,
			"message":   detail.Message,
			"operation": detail.Operation,
			"files":     files,
		})
	}
}

// handleActivity handles GET /api/v1/{repo}/activity?path=<path>.
// Returns commit-activity metrics (last change, total commits, 7d/30d/90d counts).
func handleActivity(agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		var gs repos.GitStore
		ri.WithRead(func(d repos.StoreDeps) { gs = d.GS })
		result, err := gs.Activity(agentBranch, r.URL.Query().Get("path"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("activity error: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// handleCompletions handles GET /api/v1/{repo}/completions?category=<cat>&prefix=<p>
// Returns autocomplete suggestions for filter values (domain, entity, type, ep, path).
func handleCompletions() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		branch := ri.Branch()
		var idx repos.SearchIndex
		ri.WithRead(func(d repos.StoreDeps) { idx = d.Idx })

		category := r.URL.Query().Get("category")
		prefix := r.URL.Query().Get("prefix")
		if category == "" {
			http.Error(w, "category required", http.StatusBadRequest)
			return
		}
		vals, err := idx.Completions(branch, category, prefix, 20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"values": vals})
	}
}

// handleStats handles GET /api/v1/{repo}/stats?path=<path>.
// Aggregates are computed with a SQL query over the search index.
func handleStats() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		branch := ri.Branch()
		var idx repos.SearchIndex
		ri.WithRead(func(d repos.StoreDeps) { idx = d.Idx })
		if idx == nil {
			writeError(w, http.StatusServiceUnavailable, "index not available")
			return
		}
		stats, err := idx.Stats(branch, r.URL.Query().Get("path"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("stats error: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, stats)
	}
}

// handleStatus handles GET /api/v1/{repo}/status
func handleStatus(embeddingsEnabled bool, ontologyRoot, agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		var gs repos.GitStore
		var idx repos.SearchIndex
		ri.WithRead(func(d repos.StoreDeps) {
			gs = d.GS
			idx = d.Idx
		})
		head, err := gs.HeadCommit(agentBranch)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("head commit error: %v", err))
			return
		}

		branch := agentBranch

		indexCommit := ""
		if idx != nil {
			indexCommit, _ = idx.GetLastCommit(branch)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"head":               head,
			"branch":             branch,
			"index_commit":       indexCommit,
			"embeddings_enabled": embeddingsEnabled,
			"ontology_root":      ontologyRoot,
		})
	}
}

// handleRecent handles GET /api/v1/{repo}/recent?path=<prefix>&q=<query>&limit=50&offset=0
// Additional filters: domain=<d1,d2>, entities=<e1,e2>, ep=<op1,op2>, type=<t1>, exclude_type=<t1>
func handleRecent() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		branch := ri.Branch()
		var svc *store.Service
		ri.WithRead(func(d repos.StoreDeps) { svc = d.Svc })
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "index not available")
			return
		}

		q := r.URL.Query()
		path := q.Get("path")
		limit := 50
		if v := q.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		offset := 0
		if v := q.Get("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				offset = n
			}
		}

		query := q.Get("q")

		var includeTypes []string
		if v := q.Get("type"); v != "" {
			for _, t := range strings.Split(v, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					includeTypes = append(includeTypes, t)
				}
			}
		}
		var excludeTypes []string
		if v := q.Get("exclude_type"); v != "" {
			for _, t := range strings.Split(v, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					excludeTypes = append(excludeTypes, t)
				}
			}
		}

		var domain []string
		if v := q.Get("domain"); v != "" {
			for _, d := range strings.Split(v, ",") {
				d = strings.TrimSpace(d)
				if d != "" {
					domain = append(domain, d)
				}
			}
		}

		var entities []string
		if v := q.Get("entities"); v != "" {
			for _, e := range strings.Split(v, ",") {
				e = strings.TrimSpace(e)
				if e != "" {
					entities = append(entities, e)
				}
			}
		}

		var epOps []string
		if v := q.Get("ep"); v != "" {
			for _, op := range strings.Split(v, ",") {
				op = strings.TrimSpace(op)
				if op != "" {
					epOps = append(epOps, op)
				}
			}
		}

		entries, total, err := svc.Index().RecentFacts(branch, path, query, limit, offset, includeTypes, excludeTypes, domain, entities, epOps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("recent error: %v", err))
			return
		}
		if entries == nil {
			entries = []store.RecentFactEntry{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"facts": entries,
			"total": total,
		})
	}
}
