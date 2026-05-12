package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// handleCommitAnchoredFact serves
// GET /repos/{repo}/branches/{branch}/commits/{sha}/facts/*.
//
// It behaves like handleHALFact but pins the anchor to a specific commit SHA.
// Sub-resources /incoming and /outgoing are both supported; /commits is not
// (a commit-anchored fact IS at a specific commit — use the branch-anchored
// /facts/.../commits for history).
func handleCommitAnchoredFact(b hal.URLBuilder, m *repos.Manager, reader FactReader, subProvider factSubProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		branch := BranchFromContext(r.Context())
		sha := chi.URLParam(r, "sha")
		path := chi.URLParam(r, "*")

		// Dispatch /incoming sub-resource (commit-anchored).
		if strings.HasSuffix(path, "/incoming") {
			factPath := strings.TrimSuffix(path, "/incoming")
			a := hal.Anchor{Branch: branch, Commit: sha}
			handleCommitAnchoredIncoming(b, m, subProvider, w, r, repoName, a, factPath)
			return
		}

		// Dispatch /outgoing sub-resource.
		if strings.HasSuffix(path, "/outgoing") {
			factPath := strings.TrimSuffix(path, "/outgoing")
			a := hal.Anchor{Branch: branch, Commit: sha}
			handleCommitAnchoredOutgoing(b, m, subProvider, w, r, repoName, a, factPath)
			return
		}

		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}
		if path == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing fact path",
				"fact path is required", r.URL.Path)
			return
		}

		a := hal.Anchor{Branch: branch, Commit: sha}
		// ?fallback=before: when the fact is missing at the pinned commit,
		// fall back to the most recent ancestor where it existed.
		fallback := r.URL.Query().Get("fallback") == "before"
		f, head, err := reader.Read(ri, a, path, fallback)
		if err != nil {
			if errors.Is(err, errFactNotFound) {
				hal.WriteProblem(w, http.StatusNotFound, "Fact not found",
					`no fact at path "`+path+`" on branch "`+branch+`" at commit "`+sha+`"`, r.URL.Path)
				return
			}
			log.Error().Err(err).Str("path", path).Str("branch", branch).Str("sha", sha).Msg("commit-anchored fact read failed")
			writeStoreError(w, r, err, "Failed to read fact", branch)
			return
		}

		// When the fallback fired, head is the actual content's source
		// commit (different from the URL's sha). Reflect that in the
		// view's anchor so as_of.commit points at the version being shown.
		viewAnchor := a
		if head != "" && head != a.Commit {
			viewAnchor = hal.Anchor{Branch: branch, Commit: head}
		}
		view := BuildFactView(b, repoName, viewAnchor, head, f, reader)
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleCommitAnchoredOutgoing serves
// GET /repos/{repo}/branches/{branch}/commits/{sha}/facts/*/outgoing.
func handleCommitAnchoredOutgoing(
	b hal.URLBuilder,
	m *repos.Manager,
	subProvider factSubProvider,
	w http.ResponseWriter,
	r *http.Request,
	repoName string,
	a hal.Anchor,
	factPath string,
) {
	ri := m.Get(repoName)
	if ri == nil {
		hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
			`no repo named "`+repoName+`"`, r.URL.Path)
		return
	}

	refs, err := subProvider.OutgoingAtCommit(ri, a.Branch, factPath, a.Commit)
	if err != nil {
		writeStoreError(w, r, err, "Failed to load outgoing refs", a.Branch)
		return
	}

	selfURL := b.FactOutgoing(repoName, a, factPath)
	items := buildGraphRefItems(b, repoName, a, refs)
	view := hal.CollectionView[graphRefEntry]{
		Count: len(items),
		Links: hal.LinkMap{"self": {Href: selfURL}},
		Embedded: map[string][]graphRefEntry{
			"refs": items,
		},
	}
	hal.WriteHAL(w, http.StatusOK, view)
}
