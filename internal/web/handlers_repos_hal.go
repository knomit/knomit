package web

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// repoSummary is the minimal shape for an item inside the /repos collection.
// Per hard rule §3 #7, embedded items carry only _links.self (plus minimal
// display fields — the name is the display field here).
type repoSummary struct {
	Name  string      `json:"name"`
	Links hal.LinkMap `json:"_links"`
}

// handleHALRepos serves GET /api/v1/repos.
func handleHALRepos(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		names := make([]string, 0)
		m.ForEach(func(name string, _ *repos.RepoInstance) {
			names = append(names, name)
		})
		sort.Strings(names) // deterministic order

		items := make([]repoSummary, 0, len(names))
		for _, name := range names {
			items = append(items, repoSummary{
				Name:  name,
				Links: hal.LinkMap{"self": {Href: b.Repo(name)}},
			})
		}

		body := hal.CollectionView[repoSummary]{
			Count:    len(items),
			Links:    hal.LinkMap{"self": {Href: b.Repos()}},
			Embedded: map[string][]repoSummary{"repos": items},
		}
		hal.WriteHAL(w, http.StatusOK, body)
	}
}

// handleHALRepo serves GET /api/v1/repos/{repo}.
func handleHALRepo(b hal.URLBuilder, m *repos.Manager, agentBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "repo")
		ri := m.Get(name)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+name+`"`, r.URL.Path)
			return
		}
		a := hal.Anchor{Branch: agentBranch}
		body := map[string]any{
			"name":         name,
			"agent_branch": agentBranch,
			"_links": hal.LinkMap{
				"self":     {Href: b.Repo(name)},
				"branches": {Href: b.Branches(name)},
				"mcp":      {Href: b.Branch(name, a) + "/mcp{?profile}", Templated: true},
			},
		}
		hal.WriteHAL(w, http.StatusOK, body)
	}
}
