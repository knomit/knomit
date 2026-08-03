package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	knomitfact "knomit/internal/fact"
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
func handleCommitAnchoredFact(b hal.URLBuilder, reader FactReader, subProvider factSubProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		branch := BranchFromContext(r.Context())
		sha := chi.URLParam(r, "sha")
		path := chi.URLParam(r, "*")

		// Dispatch /incoming sub-resource (commit-anchored).
		if strings.HasSuffix(path, "/incoming") {
			factPath := strings.TrimSuffix(path, "/incoming")
			a := hal.Anchor{Branch: branch, Commit: sha}
			handleCommitAnchoredIncoming(b, subProvider, w, r, repoName, a, factPath)
			return
		}

		// Dispatch /outgoing sub-resource.
		if strings.HasSuffix(path, "/outgoing") {
			factPath := strings.TrimSuffix(path, "/outgoing")
			a := hal.Anchor{Branch: branch, Commit: sha}
			handleCommitAnchoredOutgoing(b, subProvider, w, r, repoName, a, factPath)
			return
		}

		ri := repos.RepoFromContext(r.Context())
		if path == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing fact path",
				"fact path is required", r.URL.Path)
			return
		}

		a := hal.Anchor{Branch: branch, Commit: sha}
		// ?fallback=before: when the fact is missing at the pinned commit,
		// fall back to the most recent ancestor where it existed.
		fallback := r.URL.Query().Get("fallback") == "before"
		f, head, err := reader.Read(r.Context(), ri, a, path, fallback)
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
		// Anchor the ref resolver to the SAME commit the view shows so
		// ref-kind classification is consistent with the displayed content
		// (walks back to find any prior valid version per the historical-
		// graph invariant).
		resolver := readerRefResolver{ctx: r.Context(), reader: reader, ri: ri, branch: branch, commit: viewAnchor.Commit}
		view := BuildFactView(b, repoName, viewAnchor, head, f, resolver, knomitfact.ID12(ri.ID()))
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleCommitAnchoredOutgoing serves
// GET /repos/{repo}/branches/{branch}/commits/{sha}/facts/*/outgoing.
func handleCommitAnchoredOutgoing(
	b hal.URLBuilder,
	subProvider factSubProvider,
	w http.ResponseWriter,
	r *http.Request,
	repoName string,
	a hal.Anchor,
	factPath string,
) {
	ri := repos.RepoFromContext(r.Context())

	if !factPresentAtCommitOr404(subProvider, w, r, ri, a, factPath) {
		return
	}

	refs, err := subProvider.OutgoingAtCommit(r.Context(), ri, a.Branch, factPath, a.Commit)
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
