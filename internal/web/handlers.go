package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"knomit/internal/git"
	"knomit/internal/mcp"
	"knomit/internal/store"
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

// handleBrowse handles GET /api/browse?path=<path>
func handleBrowse(gs GitStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			path = "know"
		}

		entries, err := gs.ListDir(path)
		if err != nil {
			writeError(w, http.StatusNotFound, fmt.Sprintf("cannot list %q: %v", path, err))
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

// handleFact handles GET /api/fact?path=<path>
func handleFact(gs GitStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			writeError(w, http.StatusBadRequest, "path query parameter is required")
			return
		}

		content, err := gs.ReadFile(path)
		if err != nil {
			writeError(w, http.StatusNotFound, fmt.Sprintf("fact not found: %v", err))
			return
		}

		fact, err := mcp.ParseFact(path, content)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("parse error: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, fact)
	}
}

// handleSearch handles GET /api/search?q=<query>&entities=<e1,e2>&domain=<d1,d2>&path=<p>&min_confidence=<f>&limit=<n>
func handleSearch(idx SearchIndex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		limitStr := q.Get("limit")

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

		limit := 50
		if limitStr != "" {
			v, err := strconv.Atoi(limitStr)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid limit value")
				return
			}
			limit = v
		}

		results, err := idx.Search(store.SearchQuery{
			Text:          text,
			Entities:      entities,
			Domain:        domain,
			Path:          path,
			MinConfidence: minConfidence,
			Limit:         limit,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("search error: %v", err))
			return
		}

		if results == nil {
			results = []store.SearchResult{}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"results": results,
		})
	}
}

// handleHistory handles GET /api/history?path=<path>
func handleHistory(gs GitStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")

		entries, err := gs.Log(path)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("log error: %v", err))
			return
		}

		if entries == nil {
			entries = []git.LogEntry{}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"entries": entries,
		})
	}
}

// handleStats handles GET /api/stats?path=<path>
func handleStats(gs GitStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pathPrefix := r.URL.Query().Get("path")

		allPaths, err := gs.ListAll()
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("list all error: %v", err))
			return
		}

		domains := make(map[string]int)
		total := 0
		var confidenceSum float64

		for _, p := range allPaths {
			if pathPrefix != "" && !strings.HasPrefix(p, pathPrefix) {
				continue
			}

			content, err := gs.ReadFile(p)
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
			"avg_confidence": avgConfidence,
		})
	}
}

// handleStatus handles GET /api/status
func handleStatus(gs GitStore, idx SearchIndex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		head, err := gs.HeadCommit()
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("head commit error: %v", err))
			return
		}

		branch := gs.Branch()

		indexCommit := ""
		if idx != nil {
			indexCommit, _ = idx.GetLastCommit()
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"head":               head,
			"branch":             branch,
			"index_commit":       indexCommit,
			"embeddings_enabled": false,
		})
	}
}

// handleSynthesizeStart handles POST /api/synthesize
func handleSynthesizeStart(synth SynthRunner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if synth == nil {
			writeError(w, http.StatusServiceUnavailable, "synthesis not available")
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("read body error: %v", err))
			return
		}

		id, err := synth.Start(string(body))
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("start synthesis error: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"id":     id,
			"status": "started",
		})
	}
}

// handleSynthesizeStatus handles GET /api/synthesize/{recipe}
func handleSynthesizeStatus(synth SynthRunner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if synth == nil {
			writeError(w, http.StatusServiceUnavailable, "synthesis not available")
			return
		}
		id := chi.URLParam(r, "recipe")

		events, done := synth.Status(id)
		if events == nil {
			events = []string{}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"id":     id,
			"events": events,
			"done":   done,
		})
	}
}

// handleSync handles POST /api/sync
func handleSync(gs GitStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := gs.Sync(nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"status": "error",
				"error":  err.Error(),
			})
			return
		}
		head, _ := gs.HeadCommit()
		msg := "already up to date"
		if result.Synced {
			msg = fmt.Sprintf("merged %d commit(s) from origin/main", result.Ahead)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"commit":  head,
			"message": msg,
		})
	}
}

// handleEvents handles GET /api/events — SSE endpoint for real-time updates.
func handleEvents(gs GitStore, idx SearchIndex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		// Send an initial status event immediately.
		head, _ := gs.HeadCommit()
		fmt.Fprintf(w, "event: status\ndata: {\"head\":\"%s\"}\n\n", head)
		flusher.Flush()

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				head, _ := gs.HeadCommit()
				fmt.Fprintf(w, "event: status\ndata: {\"head\":\"%s\"}\n\n", head)
				flusher.Flush()
			}
		}
	}
}
