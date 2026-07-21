package web

import (
	"net/http"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// handleCommitAnchoredIncoming serves
// GET /repos/{repo}/branches/{branch}/commits/{sha}/facts/*/incoming.
//
// Returns the lineage of the fact at this version: every (path, commit)
// whose ref to this path resolved to this version at the time it was
// written.
func handleCommitAnchoredIncoming(
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

	if !factPresentAtCommitOr404(subProvider, w, r, ri, a, factPath) {
		return
	}

	refs, err := subProvider.IncomingAtCommit(r.Context(), ri, a.Branch, factPath, a.Commit)
	if err != nil {
		writeStoreError(w, r, err, "Failed to load incoming refs", a.Branch)
		return
	}

	selfURL := b.FactIncoming(repoName, a, factPath)
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
