package web

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// repoSummary is the minimal shape for an item inside the /repos collection.
// Per hard rule §3 #7, embedded items carry only _links.self (plus minimal
// display fields — the name is the display field here).
type repoSummary struct {
	Name  string      `json:"name"`
	ID    string      `json:"id"`
	Links hal.LinkMap `json:"_links"`
}

// handleHALRepos serves GET /api/v1/repos.
func handleHALRepos(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		names := make([]string, 0)
		instances := make(map[string]*repos.RepoInstance)
		m.ForEach(func(name string, ri *repos.RepoInstance) {
			names = append(names, name)
			instances[name] = ri
		})
		sort.Strings(names) // deterministic order

		items := make([]repoSummary, 0, len(names))
		for _, name := range names {
			items = append(items, repoSummary{
				Name:  name,
				ID:    instances[name].ShortID(),
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

// readKBManifest returns the verbatim content of kb.md at the root of the
// repo's git store, read at HEAD (the agent branch tip). It returns "" if the
// store is not yet open, the branch is unknown, or kb.md does not exist — all
// non-fatal: the repo simply has no description to surface.
func readKBManifest(r *http.Request, ri *repos.RepoInstance) string {
	branch := ri.AgentBranch()
	if branch == "" {
		return ""
	}
	var content string
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		res, err := svc.Facts().ReadFact(r.Context(), branch, "kb.md", nil)
		if err != nil {
			return
		}
		content = res.Content
	})
	return content
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
func handleHALRepo(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())
		// Read the branch from the instance so the advertised agent_branch and
		// the branch readKBManifest reads kb.md from can never drift apart.
		branch := ri.AgentBranch()
		a := hal.Anchor{Branch: branch}
		body := map[string]any{
			"name":         name,
			"id":           ri.ShortID(),
			"agent_branch": branch,
			"_links": hal.LinkMap{
				"self":     {Href: b.Repo(name)},
				"branches": {Href: b.Branches(name)},
				"mcp":      {Href: b.Branch(name, a) + "/mcp{?profile}", Templated: true},
			},
		}
		// description is the verbatim kb.md root manifest read at HEAD (the
		// repo's agent branch tip — HEAD points there). Omitted when the
		// store is unreadable or kb.md is absent, so the UI shows it only
		// when available.
		if desc := readKBManifest(r, ri); desc != "" {
			body["description"] = desc
		}
		hal.WriteHAL(w, http.StatusOK, body)
	}
}
