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
//
// uid is the registry primary key. It is here because it is the ONLY spelling
// the lens API accepts for a member repo, so this collection has to be the place
// a client learns it — the lens 400 for an unknown member sends callers here by
// name. It is distinct from id, the root-commit identity `kb://<id12>/…` paths
// address a repo by: uid exists before a repo has ever been opened and survives
// a store swap, id does neither.
//
// state says whether this row has a live store: "active", or the reason it has
// none ("missing" / "unopenable" / "conflict"). It is a plain string rather than
// an enum because the web layer consumes it as one, and because the reasons are
// OBSERVED at open time — a client that meets one it does not recognise should
// show it, not drop the repo. detail is the human-readable amplification, and is
// omitted for an active repo, which has nothing to explain.
type repoSummary struct {
	Name   string      `json:"name"`
	UID    string      `json:"uid"`
	ID     string      `json:"id"`
	State  string      `json:"state"`
	Detail string      `json:"detail,omitempty"`
	Links  hal.LinkMap `json:"_links"`
}

// repoStateActive is the state of a repo that has a live store. Every other
// value of repoSummary.State is a repos.Unavailable reason.
const repoStateActive = "active"

// handleHALRepos serves GET /api/v1/repos.
//
// The collection is registered repos, NOT open ones. A repo whose database is
// missing or unopenable used to vanish from this list entirely — one ERROR line
// in the log was its only trace — so a user could not tell a repo that had been
// deleted from one that had merely failed to open. control.db now knows what
// exists independently of what opens, and this merges the two: one list, sorted
// by name, with a state on every row.
//
// The merge happens HERE and not in the Manager. Unavailable repos deliberately
// never enter m.repos: every consumer of Get/ForEach/Names relies on "this has a
// live store", and the presentation problem of listing them is not a reason to
// break that.
func handleHALRepos(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		names := make([]string, 0)
		instances := make(map[string]*repos.RepoInstance)
		m.ForEach(func(name string, ri *repos.RepoInstance) {
			names = append(names, name)
			instances[name] = ri
		})

		items := make([]repoSummary, 0, len(names))
		for _, name := range names {
			items = append(items, repoSummary{
				Name:  name,
				UID:   instances[name].UID(),
				ID:    instances[name].ShortID(),
				State: repoStateActive,
				Links: hal.LinkMap{"self": {Href: b.Repo(name)}},
			})
		}
		for _, u := range m.Unavailable() {
			// No id: the root-commit identity is a property of a store this repo
			// does not have. Reporting the registry's last-known value would be a
			// claim about content nothing here can read.
			items = append(items, repoSummary{
				Name:   u.Record.Name,
				UID:    u.Record.UID,
				State:  u.Reason,
				Detail: u.Detail,
				// The self link is real and answers 409 with the reason — this is
				// the row's only route to a fuller explanation.
				Links: hal.LinkMap{"self": {Href: b.Repo(u.Record.Name)}},
			})
		}
		// One sorted list rather than live repos with the broken ones appended:
		// the reader is looking for a name, and where it sits must not depend on
		// whether its file happens to open today.
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

		body := hal.CollectionView[repoSummary]{
			Count: len(items),
			Links: hal.LinkMap{
				"self": {Href: b.Repos()},
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

// readLicense returns the verbatim LICENSE at the repo's agent-branch tip, or
// "" when there is none — non-fatal for a view, exactly like readReadme.
func readLicense(r *http.Request, ri *repos.RepoInstance) string {
	content, err := ri.ReadLicense(r.Context())
	if err != nil {
		return ""
	}
	return content
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
		"uid":          ri.UID(),
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
	// license is the verbatim LICENSE read at HEAD. Single-repo GET only, the
	// same scoping description has — the repo LIST stays a cheap index and must
	// not grow a second per-repo git read.
	if lic := readLicense(r, ri); lic != "" {
		body["license"] = lic
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

// patchRepoRequest is the PATCH /repos/{repo} body. Description and License are
// the editable fields — name has its own action (POST /repos/{repo}/rename,
// because a rename invalidates this URL), and id and agent_branch are derived
// from the repo itself.
//
// Pointers distinguish "omitted" (keep) from "" (empty the file), per field
// independently: a patch may edit one and leave the other untouched.
type patchRepoRequest struct {
	Description *string `json:"description"`
	License     *string `json:"license"`
}

// maxRepoPatchBodyBytes bounds what the decoder will buffer, so the size check
// on the decoded description is not preceded by an unbounded read. The headroom
// over MaxRepoDescriptionBytes covers JSON escaping: a control character costs
// 6 bytes on the wire (\u00XX), so 8× can never reject a description the
// content cap would have accepted — this limit only stops bodies that are
// nowhere near a legitimate manifest.
const maxRepoPatchBodyBytes = 8 * repos.MaxRepoDescriptionBytes

// handleHALRepoPatch serves PATCH /api/v1/repos/{repo}. It edits the repo's
// description and/or license by committing README.md and/or LICENSE on the
// agent branch, then returns the same view GET does — re-read from git, so
// the response reflects what was actually persisted rather than what was
// requested.
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
		if req.Description == nil && req.License == nil {
			hal.WriteHAL(w, http.StatusOK, repoView(b, r, name, ri))
			return
		}

		// Validate BOTH lengths before writing EITHER. The two writes are two
		// separate git commits and there is no transaction spanning them, so a
		// patch carrying a fine description and an over-cap license would
		// otherwise commit README.md and only then answer 422 — the client sees
		// an error, reasonably concludes nothing happened, and is wrong. Both
		// fields share one cap (WriteLicense reuses ErrRepoDescriptionTooLong on
		// purpose) and both lengths are knowable before any git work, so the
		// half-applied outcome is avoidable outright rather than compensated.
		//
		// This does NOT make the pair atomic in general — a store fault on the
		// second write still leaves the first committed — it removes the only
		// failure mode that is both fully predictable up front and reachable by
		// an ordinary request. The per-write checks below stay: WriteReadme and
		// WriteLicense own the cap for every OTHER caller, and this handler must
		// not become the only place it is enforced.
		if req.Description != nil && len(*req.Description) > repos.MaxRepoDescriptionBytes {
			hal.WriteProblem(w, http.StatusUnprocessableEntity, "Repo description too long",
				fmt.Sprintf("description is %d bytes (max %d)",
					len(*req.Description), repos.MaxRepoDescriptionBytes), r.URL.Path)
			return
		}
		if req.License != nil && len(*req.License) > repos.MaxRepoDescriptionBytes {
			hal.WriteProblem(w, http.StatusUnprocessableEntity, "License too long",
				fmt.Sprintf("license is %d bytes (max %d)",
					len(*req.License), repos.MaxRepoDescriptionBytes), r.URL.Path)
			return
		}

		// The cap, the branch check and the unchanged-content skip all live in
		// WriteReadme, so every writer of README.md gets them; this maps its
		// errors onto status codes.
		if req.Description != nil {
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
		}
		if req.License != nil {
			// Same cap, same sentinel, same branch check as the description —
			// WriteLicense reuses ErrRepoDescriptionTooLong deliberately, so
			// this maps identically.
			if _, err := ri.WriteLicense(r.Context(), *req.License); err != nil {
				switch {
				case errors.Is(err, repos.ErrRepoDescriptionTooLong):
					hal.WriteProblem(w, http.StatusUnprocessableEntity, "License too long",
						err.Error(), r.URL.Path)
				case errors.Is(err, repos.ErrAgentBranchUnset):
					hal.WriteProblem(w, http.StatusServiceUnavailable, "Repo not ready",
						"the repo has no agent branch yet", r.URL.Path)
				case errors.Is(err, repos.ErrRepoClosed), errors.Is(err, repos.ErrStoreUnavailable):
					log.Warn().Err(err).Str("path", r.URL.Path).Str("repo", name).Msg("write LICENSE: store not available")
					hal.WriteProblem(w, http.StatusServiceUnavailable, "Repo not ready",
						"the repo's store is not available; try again", r.URL.Path)
				default:
					log.Error().Err(err).Str("path", r.URL.Path).Str("repo", name).Msg("write LICENSE failed")
					hal.WriteProblem(w, http.StatusInternalServerError, "Update failed",
						"could not write the repo license", r.URL.Path)
				}
				return
			}
		}
		hal.WriteHAL(w, http.StatusOK, repoView(b, r, name, ri))
	}
}
