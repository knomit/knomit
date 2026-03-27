package web

import (
	"net/http"

	"knomit/internal/repos"
)

func handleExplain() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		ri.RLock()
		defer ri.RUnlock()

		path := r.URL.Query().Get("path")
		if path == "" {
			writeError(w, http.StatusBadRequest, "path query parameter is required")
			return
		}
		if ri.Idx == nil {
			writeError(w, http.StatusBadRequest, "index not available")
			return
		}

		result, err := ri.Idx.ExplainFact(path)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, result)
	}
}
