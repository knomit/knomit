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
			Count: len(items),
			Links: hal.LinkMap{
				"self":   {Href: b.Repos()},
				"rescan": {Href: b.Repos() + ":rescan"},
			},
			Embedded: map[string][]repoSummary{"repos": items},
		}
		hal.WriteHAL(w, http.StatusOK, body)
	}
}

// rescanErrorView is the JSON shape for a per-repo failure entry in a
// rescan response.
type rescanErrorView struct {
	Repo  string `json:"repo"`
	Error string `json:"error"`
}

// rescanResultView is the JSON body of POST /repos:rescan. All slices
// serialize as [] (never null) so clients can iterate without nil checks.
type rescanResultView struct {
	Added   []string          `json:"added"`
	Skipped []string          `json:"skipped"`
	Errors  []rescanErrorView `json:"errors"`
	Links   hal.LinkMap       `json:"_links"`
}

// handleHALReposRescan serves POST /api/v1/repos:rescan. It triggers a
// runtime rescan of the repos directory and returns what was added,
// skipped, and what failed. Top-level scan failures (e.g. directory
// unreadable) yield 500 problem+json; per-repo Add failures appear in
// the response's errors[] with status 200.
func handleHALReposRescan(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := m.Rescan()
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Rescan Failed", err.Error(), r.URL.Path)
			return
		}

		errs := make([]rescanErrorView, 0, len(result.Errors))
		for _, e := range result.Errors {
			errs = append(errs, rescanErrorView{Repo: e.Repo, Error: e.Err.Error()})
		}

		view := rescanResultView{
			Added:   result.Added,
			Skipped: result.Skipped,
			Errors:  errs,
			Links: hal.LinkMap{
				"self":  {Href: b.Repos() + ":rescan"},
				"repos": {Href: b.Repos()},
			},
		}
		hal.WriteHAL(w, http.StatusOK, view)
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
