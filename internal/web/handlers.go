package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"knomit/internal/git"
	"knomit/internal/mcp"
	"knomit/internal/store"
	"knomit/internal/synthesize"
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
		log.Debug().Str("path", path).Msg("browse")

		entries, err := gs.ListDir(path)
		if err != nil {
			log.Debug().Err(err).Str("path", path).Msg("browse failed")
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
		log.Debug().Str("path", path).Msg("fact")

		content, err := gs.ReadFile(path)
		if err != nil {
			log.Debug().Err(err).Str("path", path).Msg("fact not found")
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
		if limit > 500 {
			limit = 500
		}

		log.Debug().Str("q", text).Strs("entities", entities).Strs("domain", domain).Int("limit", limit).Msg("search")

		results, err := idx.Search(store.SearchQuery{
			Text:          text,
			Entities:      entities,
			Domain:        domain,
			Path:          path,
			MinConfidence: minConfidence,
			Limit:         limit,
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
		entities := make(map[string]int)
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

// handleStatus handles GET /api/status
func handleStatus(gs GitStore, idx SearchIndex, embeddingsEnabled bool) http.HandlerFunc {
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
			"embeddings_enabled": embeddingsEnabled,
		})
	}
}

// writeTaskStarted writes a 200 response for a successfully started task.
func writeTaskStarted(w http.ResponseWriter, op, id string) {
	writeJSON(w, http.StatusOK, map[string]any{"op": op, "id": id, "status": "running"})
}

// writeTaskConflict writes a 409 response when a task is already running.
func writeTaskConflict(w http.ResponseWriter, op string, err error) {
	writeJSON(w, http.StatusConflict, map[string]any{"op": op, "status": "error", "message": err.Error()})
}

// handleSynthesizeStart handles POST /api/v1/synthesize
func handleSynthesizeStart(deps *SynthDeps, hub *TaskHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps == nil || deps.Adapter == nil {
			log.Warn().Msg("synthesize: not available (no LLM configured)")
			writeError(w, http.StatusServiceUnavailable, "synthesis not available")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("read body error: %v", err))
			return
		}

		// Parse recipe before starting task — bad recipe gets a 400, not an async error.
		recipeYAML := string(body)
		if recipeYAML == "" {
			recipeYAML = defaultRecipe
		}
		recipe, err := synthesize.ParseRecipe(recipeYAML)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid recipe: %v", err))
			return
		}

		log.Info().Str("recipe", recipe.Name).Msg("synthesize: starting")

		var emb synthesize.Embedder
		if deps.Embedder != nil {
			emb = deps.Embedder
		}

		id, err := hub.Start("synth", func(ctx context.Context, emit func(TaskEvent)) {
			emit(TaskEvent{Status: "running", Phase: "start", Message: "synthesis starting"})
			onProgress := func(ev synthesize.ProgressEvent) {
				emit(TaskEvent{Status: "running", Phase: ev.Phase, Message: ev.Message})
			}
			if err := synthesize.Run(ctx, deps.GS, deps.Idx, emb, deps.Adapter, recipe, onProgress); err != nil {
				emit(TaskEvent{Status: "error", Message: err.Error()})
				return
			}
			emit(TaskEvent{Status: "done", Message: "synthesis complete"})
		})
		if err != nil {
			writeTaskConflict(w, "synth", err)
			return
		}

		writeTaskStarted(w, "synth", id)
	}
}

const defaultRecipe = `name: default
prompt: Review and consolidate the knowledge base.
steps:
  - mode: prune
    prompt: Identify stale, redundant, or outdated facts.
`

// handleSync handles POST /api/v1/sync
func handleSync(gs GitStore, hub *TaskHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := hub.Start("sync", func(ctx context.Context, emit func(TaskEvent)) {
			emit(TaskEvent{Status: "running", Message: "syncing"})
			result, err := gs.Sync(nil)
			if err != nil {
				emit(TaskEvent{Status: "error", Message: err.Error()})
				return
			}
			head, _ := gs.HeadCommit()
			msg := "already up to date"
			if result.Synced {
				msg = fmt.Sprintf("merged %d commit(s) from origin/main", result.Ahead)
			}
			emit(TaskEvent{Status: "done", Message: fmt.Sprintf("%s (%s)", msg, head[:min(7, len(head))])})
		})
		if err != nil {
			writeTaskConflict(w, "sync", err)
			return
		}

		writeTaskStarted(w, "sync", id)
	}
}

// handleEvents handles GET /api/v1/events — SSE endpoint for real-time updates.
func handleEvents(gs GitStore, idx SearchIndex, hub *TaskHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		// Subscribe and get snapshot atomically.
		events, snapshot := hub.Subscribe(r.Context())

		// Send initial status event.
		head, _ := gs.HeadCommit()
		fmt.Fprintf(w, "event: status\ndata: {\"head\":\"%s\"}\n\n", head)

		// Replay snapshot (reconnect recovery).
		for _, ev := range snapshot {
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: task\ndata: %s\n\n", data)
		}
		flusher.Flush()

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-events:
				if !ok {
					return
				}
				ev, isTask := e.(TaskEvent)
				if !isTask {
					continue
				}
				data, _ := json.Marshal(ev)
				fmt.Fprintf(w, "event: task\ndata: %s\n\n", data)
				flusher.Flush()
			case <-ticker.C:
				head, _ := gs.HeadCommit()
				fmt.Fprintf(w, "event: status\ndata: {\"head\":\"%s\"}\n\n", head)
				flusher.Flush()
			}
		}
	}
}
