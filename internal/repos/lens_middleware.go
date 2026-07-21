package repos

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// LensMiddleware resolves the {lens} URL param via the Manager's registry,
// builds the Binding, and stores it (plus the write repo as the context
// RepoInstance, for repo-based paths like session instructions) in the
// request context. Resolution fails loudly: an unknown lens is 404; a lens
// referencing an unavailable repo is 503 — a lens never silently shrinks
// its read set (lenses RFC §9.1).
func LensMiddleware(m *Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := chi.URLParam(r, "lens")
			reg := m.Registry()
			if reg == nil {
				writeLensErr(w, http.StatusServiceUnavailable, "lens registry not started")
				return
			}
			l, ok, err := reg.Get(name)
			if err != nil {
				writeLensErr(w, http.StatusInternalServerError, "lens registry: "+err.Error())
				return
			}
			if !ok {
				writeLensErr(w, http.StatusNotFound, "lens not found: "+name)
				return
			}
			b, err := NewBindingOfLens(m, l)
			if err != nil {
				writeLensErr(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			ctx := WithBinding(r.Context(), b)
			ctx = WithRepoInstance(ctx, b.Write())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeLensErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
