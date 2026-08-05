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

// readReadme returns the verbatim content of README.md at the root of the
// repo's git store, read at HEAD (the agent branch tip). It returns "" if the
// store is not yet open, the branch is unknown, or README.md does not exist —
// all non-fatal for a view: the repo simply has no description to surface.
func readReadme(r *http.Request, ri *repos.RepoInstance) string {
	content, err := ri.ReadReadme(r.Context())
	if err != nil {
		return ""
	}
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
	// the branch readReadme reads README.md from can never drift apart.
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
	// description is the verbatim README.md root manifest read at HEAD (the
	// repo's agent branch tip — HEAD points there). Omitted when the
	// store is unreadable or README.md is absent, so the UI shows it only
	// when available.
	if desc := readReadme(r, ri); desc != "" {
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

// patchRepoRequest is the PATCH /repos/{repo} body. Only description is
// editable — name, id and agent_branch are all derived from the repo itself.
// A pointer distinguishes "omitted" (keep) from "" (empty the manifest).
type patchRepoRequest struct {
	Description *string `json:"description"`
}

// maxRepoPatchBodyBytes bounds what the decoder will buffer, so the size check
// on the decoded description is not preceded by an unbounded read. The headroom
// over MaxRepoDescriptionBytes covers JSON escaping: a control character costs
// 6 bytes on the wire (\u00XX), so 8× can never reject a description the
// content cap would have accepted — this limit only stops bodies that are
// nowhere near a legitimate manifest.
const maxRepoPatchBodyBytes = 8 * repos.MaxRepoDescriptionBytes

// handleHALRepoPatch serves PATCH /api/v1/repos/{repo}. It edits the repo's
// description by committing README.md on the agent branch, then returns the
// same view GET does — re-read from git, so the response reflects what was
// actually persisted rather than what was requested.
func handleHALRepoPatch(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		r.Body = http.MaxBytesReader(w, r.Body, maxRepoPatchBodyBytes)
		var req patchRepoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				hal.WriteProblem(w, http.StatusRequestEntityTooLarge, "Request body too large",
					fmt.Sprintf("the request body exceeds %d bytes", maxRepoPatchBodyBytes), r.URL.Path)
				return
			}
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body", err.Error(), r.URL.Path)
			return
		}
		// Nothing to do — an empty patch is a successful no-op, not an error.
		if req.Description == nil {
			hal.WriteHAL(w, http.StatusOK, repoView(b, r, name, ri))
			return
		}
		// The cap, the branch check and the unchanged-content skip all live in
		// WriteReadme, so every writer of README.md gets them; this maps its
		// errors onto status codes.
		if _, err := ri.WriteReadme(r.Context(), *req.Description); err != nil {
			switch {
			case errors.Is(err, repos.ErrRepoDescriptionTooLong):
				hal.WriteProblem(w, http.StatusUnprocessableEntity, "Repo description too long",
					err.Error(), r.URL.Path)
			case errors.Is(err, repos.ErrAgentBranchUnset):
				hal.WriteProblem(w, http.StatusServiceUnavailable, "Repo not ready",
					"the repo has no agent branch yet", r.URL.Path)
			case errors.Is(err, repos.ErrRepoClosed), errors.Is(err, repos.ErrStoreUnavailable):
				log.Warn().Err(err).Str("path", r.URL.Path).Str("repo", name).Msg("write README.md: store not available")
				hal.WriteProblem(w, http.StatusServiceUnavailable, "Repo not ready",
					"the repo's store is not available; try again", r.URL.Path)
			default:
				log.Error().Err(err).Str("path", r.URL.Path).Str("repo", name).Msg("write README.md failed")
				hal.WriteProblem(w, http.StatusInternalServerError, "Update failed",
					"could not write the repo description", r.URL.Path)
			}
			return
		}
		hal.WriteHAL(w, http.StatusOK, repoView(b, r, name, ri))
	}
}
