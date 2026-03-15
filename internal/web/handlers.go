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
		var err error
		if commitHash != "" {
			content, err = ri.GS.ReadFileAtCommit(path, commitHash)
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
			// Not a fact file (e.g. kb.md manifest) — return raw content.
			writeJSON(w, http.StatusOK, map[string]any{
				"path":  path,
				"title": path,
				"body":  content,
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

// handleStats handles GET /api/v1/{repo}/stats?path=<path>.
// It iterates over all facts in the knowledge base (optionally filtered
// by path prefix), collecting domain/entity counts and average confidence.
// This is fine for small-to-medium knowledge bases; very large repos may
// want a cached or incremental approach.
func handleStats() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
		pathPrefix := r.URL.Query().Get("path")

		allPaths, err := ri.GS.ListAll()
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("list all error: %v", err))
			return
		}

		domains := make(map[string]int)
		entities := make(map[string]int)
		total := 0
		var confidenceSum float64

		for _, p := range allPaths {
			if pathPrefix != "" && !strings.HasPrefix(p, pathPrefix) {
				continue
			}

			content, err := ri.GS.ReadFile(p)
			if err != nil {
				continue
			}

			fact, err := mcp.ParseFact(p, content)
			if err != nil {
				continue
			}

			total++
			confidenceSum += fact.Confidence
			for _, d := range fact.Domain {
				domains[d]++
			}
			for _, e := range fact.Entities {
				entities[e]++
			}
		}

		avgConfidence := 0.0
		if total > 0 {
			avgConfidence = confidenceSum / float64(total)
			// Round to 2 decimal places.
			avgConfidence = float64(int(avgConfidence*100+0.5)) / 100
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"total":          total,
			"domains":        domains,
			"entities":       entities,
			"avg_confidence": avgConfidence,
		})
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
