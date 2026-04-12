package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// handleCommitAnchoredFact serves
// GET /repos/{repo}/branches/{branch}/commits/{sha}/facts/*.
//
// It behaves like handleHALFact but pins the anchor to a specific commit SHA.
// Sub-resource dispatch is limited to /outgoing only — commit-anchored views
// do not expose /incoming (per design spec §5B). /commits is also not
// available on commit-anchored fact views (a commit-anchored fact IS at a
// specific commit; use the branch-anchored /facts/.../commits for history).
func handleCommitAnchoredFact(b hal.URLBuilder, m *repos.Manager, reader FactReader, subProvider factSubProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		branch := BranchFromContext(r.Context())
		sha := chi.URLParam(r, "sha")
		path := chi.URLParam(r, "*")

		// Commit-anchored views: /incoming is not supported (design spec §5B).
		if strings.HasSuffix(path, "/incoming") {
			hal.WriteProblem(w, http.StatusNotFound, "Not Found",
				"incoming edges are not available on commit-anchored views", r.URL.Path)
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
		f, head, err := reader.Read(ri, a, path)
		if err != nil {
			if errors.Is(err, errFactNotFound) {
				hal.WriteProblem(w, http.StatusNotFound, "Fact not found",
					`no fact at path "`+path+`" on branch "`+branch+`" at commit "`+sha+`"`, r.URL.Path)
				return
			}
			hal.WriteProblem(w, http.StatusInternalServerError,
				"Failed to read fact", err.Error(), r.URL.Path)
			return
		}

		view := BuildFactView(b, repoName, a, head, f, reader)
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleCommitAnchoredOutgoing serves the outgoing graph for a fact at a
// specific commit. Uses ExplainFact as a pragmatic approximation — it reads
// from the current index rather than re-computing the graph from the commit
// blob.
//
// TODO(Plan 02): replace with OutgoingFromBlob(ri, a.Branch, a.Commit, factPath)
// once the store exposes true commit-anchored graph reads.
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

	result, err := subProvider.ExplainFact(ri, a.Branch, factPath)
	if err != nil {
		hal.WriteProblem(w, http.StatusInternalServerError,
			"Failed to load outgoing refs", err.Error(), r.URL.Path)
		return
	}

	selfURL := b.FactOutgoing(repoName, a, factPath)
	items := buildGraphRefItems(b, repoName, a, result.Outgoing)
	view := hal.CollectionView[graphRefEntry]{
		Count: len(items),
		Links: hal.LinkMap{"self": {Href: selfURL}},
		Embedded: map[string][]graphRefEntry{
			"refs": items,
		},
	}
	hal.WriteHAL(w, http.StatusOK, view)
}
