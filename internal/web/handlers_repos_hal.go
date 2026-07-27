package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// errRepoStoreUnavailable is returned when a repo's git store is not open, so
// its kb.md manifest can be neither read nor written.
var errRepoStoreUnavailable = errors.New("repo store unavailable")

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

// repoView builds the single-repo body. GET and PATCH share it so a write's
// response is byte-identical to a subsequent read — the client never has to
// reconcile two shapes for the same resource.
func repoView(b hal.URLBuilder, r *http.Request, name string, ri *repos.RepoInstance) map[string]any {
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
	return body
}

// handleHALRepo serves GET /api/v1/repos/{repo}.
func handleHALRepo(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())
		hal.WriteHAL(w, http.StatusOK, repoView(b, r, name, ri))
	}
}

// MaxRepoDescriptionBytes caps a repo description. It is deliberately far
// larger than MaxLensDescriptionBytes: a lens description is a one-line note
// about a read union, whereas kb.md is a repo's root manifest and routinely
// runs to several pages of guidance for the agents reading it.
const MaxRepoDescriptionBytes = 64 * 1024

// patchRepoRequest is the PATCH /repos/{repo} body. Only description is
// editable — name, id and agent_branch are all derived from the repo itself.
// A pointer distinguishes "omitted" (keep) from "" (clear the manifest).
type patchRepoRequest struct {
	Description *string `json:"description"`
}

// writeKBManifest commits `content` to kb.md at the root of the repo's git
// store on the agent branch — the exact file and branch readKBManifest reads,
// so an edit round-trips through GET. kb.md is not a fact: the search indexer
// skips it as unparseable (see search_index.go), so this only moves the file
// and its commit-log entry, never the fact index.
func writeKBManifest(r *http.Request, ri *repos.RepoInstance, branch, content string) error {
	var err error
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errRepoStoreUnavailable
			return
		}
		_, err = svc.Facts().WriteFact(r.Context(), branch, "kb.md",
			content, "docs: update kb.md root manifest", "update")
	})
	return err
}

// handleHALRepoPatch serves PATCH /api/v1/repos/{repo}. It edits the repo's
// description by committing kb.md on the agent branch, then returns the same
// view GET does — re-read from git, so the response reflects what was actually
// persisted rather than what was requested.
func handleHALRepoPatch(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		var req patchRepoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body", err.Error(), r.URL.Path)
			return
		}
		// Nothing to do — an empty patch is a successful no-op, not an error.
		if req.Description == nil {
			hal.WriteHAL(w, http.StatusOK, repoView(b, r, name, ri))
			return
		}
		if len(*req.Description) > MaxRepoDescriptionBytes {
			hal.WriteProblem(w, http.StatusUnprocessableEntity, "Repo description too long",
				fmt.Sprintf("description is %d bytes; the maximum is %d",
					len(*req.Description), MaxRepoDescriptionBytes), r.URL.Path)
			return
		}
		branch := ri.AgentBranch()
		if branch == "" {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Repo not ready",
				"the repo has no agent branch yet", r.URL.Path)
			return
		}
		if err := writeKBManifest(r, ri, branch, *req.Description); err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Str("repo", name).Msg("write kb.md failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "Update failed",
				"could not write the repo description", r.URL.Path)
			return
		}
		hal.WriteHAL(w, http.StatusOK, repoView(b, r, name, ri))
	}
}
