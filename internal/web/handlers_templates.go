package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// handleTemplates serves GET /api/v1/templates (F24): every template of every
// mounted, open repo, as {"templates": [{repo, name, description, commit,
// fact}]}. A template is listed when its `part: template` fact and its folder
// both exist at the repo's consensus tip; description is the fact's title.
// A repo that is not open is left out, never an error: the listing is a
// picker, and one broken mount must not empty it.
func handleTemplates(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := m.ListTemplates(r.Context(), "")
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Templates unavailable", err.Error(), r.URL.Path)
			return
		}
		writeTemplates(w, out)
	}
}

// handleRepoTemplates serves GET /api/v1/repos/{repo}/templates: the same for
// one repo, for a script that knows the source. RepoMiddleware has already
// answered 404 for an unknown name (a lens name included) and 409 for a
// registered repo with no store; a repo still populating is 503 here.
func handleRepoTemplates(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		out, err := m.ListTemplates(r.Context(), ri.Name())
		if err != nil {
			if errors.Is(err, repos.ErrTemplateSourceUnavailable) {
				hal.WriteProblem(w, http.StatusServiceUnavailable, "Template source unavailable", err.Error(), r.URL.Path)
				return
			}
			hal.WriteProblem(w, http.StatusInternalServerError, "Templates unavailable", err.Error(), r.URL.Path)
			return
		}
		writeTemplates(w, out)
	}
}

func writeTemplates(w http.ResponseWriter, out []repos.TemplateInfo) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"templates": out})
}
