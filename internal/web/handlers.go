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
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"knomit/internal/git"
	"knomit/internal/mcp"
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
func handleBrowse(ontologyRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
		path := r.URL.Query().Get("path")
		if path == "" {
			path = ontologyRoot
		}

		entries, err := ri.GS.ListDir(path)
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
		}
		children := make([]child, 0, len(entries))
		for _, e := range entries {
			children = append(children, child{Name: e.Name, IsDir: e.IsDir})
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"path":     path,
			"children": children,
		})
	}
}

// handleFact handles GET /api/v1/{repo}/fact?path=<path>&commit=<hash>
func handleFact() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
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
			content, err = ri.GS.ReadFileAtCommit(path, commitHash)
			if err != nil {
				// File may have been deleted in this commit (e.g. retract).
				// Fall back to the last commit where the file existed.
				content, fromCommit, err = ri.GS.ReadFileLastCommit(path, commitHash)
			}
		} else {
			content, err = ri.GS.ReadFile(path)
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

		if fromCommit != "" {
			writeJSON(w, http.StatusOK, map[string]any{
				"path":        fact.Path,
				"title":       fact.Title,
				"type":        fact.Type,
				"body":        fact.Body,
				"domain":      fact.Domain,
				"confidence":  fact.Confidence,
				"sources":     fact.Sources,
				"entities":    fact.Entities,
				"refs":        fact.Refs,
				"from_commit": fromCommit,
			})
			return
		}

		// Browsing mode: enrich with commit hash and date from the store index.
		if commitHash == "" && ri.Svc != nil {
			if rec, lerr := ri.Svc.Index().GetByPath(path); lerr == nil && rec != nil && rec.CommitHash != "" {
				resp := map[string]any{
					"path":        fact.Path,
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
				var ts sql.NullInt64
				if qerr := ri.Svc.DB().QueryRow(
					`SELECT committed_at FROM commit_log WHERE commit_hash = ? LIMIT 1`,
					rec.CommitHash,
				).Scan(&ts); qerr == nil && ts.Valid {
					resp["commit_date"] = time.Unix(ts.Int64, 0).UTC().Format(time.RFC3339)
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
func handleFactWrite() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())

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
		if _, _, err := ri.GS.WriteFile(req.Path, req.Content, msg, "update"); err != nil {
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

// handleSearch handles GET /api/v1/{repo}/search?q=<query>&entities=<e1,e2>&domain=<d1,d2>&path=<p>&min_confidence=<f>&limit=<n>.
// The entities and domain filters are AND-combined (all specified values
// must match). Each accepts a comma-separated list of terms.
func handleSearch() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
		if ri.Idx == nil {
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

		log.Debug().Str("q", text).Strs("entities", entities).Strs("domain", domain).Int("limit", limit).Msg("search")

		results, err := ri.Idx.Search(store.SearchQuery{
			Text:          text,
			Entities:      entities,
			Domain:        domain,
			Path:          path,
			MinConfidence: minConfidence,
			MinSimilarity: minSimilarity,
			Limit:         limit,
			GraphHops:     graphHops,
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
func handleHistoryPaginated() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
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

		entries, next, err := ri.GS.LogPaginated(path, limit, after)
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
		writeJSON(w, http.StatusOK, resp)
	}
}

// handleCommitDetail handles GET /api/v1/{repo}/commit?hash=<hash>
func handleCommitDetail() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
		hash := r.URL.Query().Get("hash")
		if hash == "" {
			writeError(w, http.StatusBadRequest, "hash query parameter is required")
			return
		}

		detail, err := ri.GS.CommitDetail(hash)
		if err != nil {
			writeError(w, http.StatusNotFound, fmt.Sprintf("commit not found: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, detail)
	}
}

// handleActivity handles GET /api/v1/{repo}/activity?path=<path>.
// Returns commit-activity metrics (last change, total commits, 7d/30d/90d counts).
func handleActivity() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
		result, err := ri.GS.Activity(r.URL.Query().Get("path"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("activity error: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// handleStats handles GET /api/v1/{repo}/stats?path=<path>.
// Aggregates are computed with a SQL query over the search index.
func handleStats() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
		if ri.Idx == nil {
			writeError(w, http.StatusServiceUnavailable, "index not available")
			return
		}
		stats, err := ri.Idx.Stats(r.URL.Query().Get("path"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("stats error: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, stats)
	}
}

// handleStatus handles GET /api/v1/{repo}/status
func handleStatus(embeddingsEnabled bool, ontologyRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
		head, err := ri.GS.HeadCommit()
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("head commit error: %v", err))
			return
		}

		branch := ri.GS.Branch()

		indexCommit := ""
		if ri.Idx != nil {
			indexCommit, _ = ri.Idx.GetLastCommit(branch)
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
