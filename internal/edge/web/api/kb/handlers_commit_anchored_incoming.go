package kb

import (
	"knomit/internal/edge/web/httpx"
	"net/http"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
)

// handleCommitAnchoredIncoming serves
// GET /repos/{repo}/branches/{branch}/commits/{sha}/facts/*/incoming.
//
// Returns the lineage of the fact at this version: every (path, commit)
// whose ref to this path resolved to this version at the time it was
// written.
func handleCommitAnchoredIncoming(
	b hal.URLBuilder,
	w http.ResponseWriter,
	r *http.Request,
	repoName string,
	a hal.Anchor,
	factPath string,
) {
	ri := repos.RepoFromContext(r.Context())

	if !factPresentAtCommitOr404(w, r, ri, a, factPath) {
		return
	}

	refs, err := incomingAtCommit(r.Context(), ri, a.Branch, factPath, a.Commit)
	if err != nil {
		httpx.WriteStoreError(w, r, err, "Failed to load incoming refs", a.Branch)
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
