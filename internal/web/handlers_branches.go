package web

import (
	"context"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// branchSummary is the minimal embedded-item shape for a branch in the
// /repos/{repo}/branches collection.
type branchSummary struct {
	Name  string      `json:"name"`
	Links hal.LinkMap `json:"_links"`
}

// handleHALBranches serves GET /api/v1/repos/{repo}/branches.
func handleHALBranches(
	b hal.URLBuilder,
	lister func(context.Context, *repos.RepoInstance) ([]store.Branch, error),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())
		branches, err := lister(r.Context(), ri)
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError,
				"Failed to list branches", err.Error(), r.URL.Path)
			return
		}

		// Deterministic order by name.
		sort.SliceStable(branches, func(i, j int) bool {
			return branches[i].Name < branches[j].Name
		})

		items := make([]branchSummary, 0, len(branches))
		for _, br := range branches {
			a := hal.Anchor{Branch: br.Name}
			items = append(items, branchSummary{
				Name:  br.Name,
				Links: hal.LinkMap{"self": {Href: b.Branch(repoName, a)}},
			})
		}

		body := hal.CollectionView[branchSummary]{
			Count:    len(items),
			Links:    hal.LinkMap{"self": {Href: b.Branches(repoName)}},
			Embedded: map[string][]branchSummary{"branches": items},
		}
		hal.WriteHAL(w, http.StatusOK, body)
	}
}

// branchRootInfo is the read-layer data bundle the branch root handler
// needs from the store. Fields map directly to the JSON envelope except
// for the link map which is built by the handler.
type branchRootInfo struct {
	Head        string
	IndexCommit string
}

// indexPercent maps the background-index status to a 0–100 completion percent
// for the API. A "ready" index is always 100; an "indexing"/"error" index
// reports done/total (0 when the total is still unknown). This saves every
// client from re-deriving the percentage and handling the total==0 edge.
func indexPercent(state string, done, total int) int {
	if state == "ready" {
		return 100
	}
	if total <= 0 {
		return 0
	}
	return min(done*100/total, 100)
}

// handleHALBranch serves GET /api/v1/repos/{repo}/branches/{branch}.
// This is the HAL entry point for a branch: the client bookmarks it and
// navigates every sub-collection via _links.
func handleHALBranch(
	b hal.URLBuilder,
	reader func(context.Context, *repos.RepoInstance, string) (branchRootInfo, error),
	agentBranch string,
	embeddingsEnabled bool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())
		branch := BranchFromContext(r.Context())
		info, err := reader(r.Context(), ri, branch)
		if err != nil {
			writeStoreError(w, r, err, "Failed to read branch", branch)
			return
		}

		idxState, idxDone, idxTotal := ri.IndexStatus()

		// ALL REPOS MUST HAVE AN ONTOLOGY. When one could not be established
		// the repo is readable but writes are refused everywhere
		// (RepoInstance.WritableBranch), and a refusal the client cannot
		// explain is indistinguishable from a bug. Reported here, on the branch
		// the client is looking at, in the same payload that already says
		// whether this is the agent branch.
		ontologyErr := ""
		if oerr := ri.OntologyError(); oerr != nil {
			ontologyErr = oerr.Error()
		}

		a := hal.Anchor{Branch: branch}
		branchURL := b.Branch(repoName, a)
		body := map[string]any{
			"name":               branch,
			"head":               info.Head,
			"index_commit":       info.IndexCommit,
			"embeddings_enabled": embeddingsEnabled,
			"is_agent_branch":    branch == agentBranch,
			"writable":           ri.WritableBranch(branch),
			"ontology_error":     ontologyErr,
			"index_state":        idxState, // "ready" | "indexing" | "error"
			"index_done":         idxDone,
			"index_total":        idxTotal,
			"index_percent":      indexPercent(idxState, idxDone, idxTotal), // 0–100; 100 when ready

			"_links": hal.LinkMap{
				"self":           {Href: branchURL},
				"facts":          {Href: branchURL + "/facts{?path,q,topic,domain,entity,type,exclude_type,kind,exclude_kind,ep,min_confidence,limit,offset}", Templated: true},
				"topics":         {Href: branchURL + "/topics"},
				"commits":        {Href: branchURL + "/commits"},
				"search":         {Href: branchURL + "/search{?q,limit,cursor}", Templated: true},
				"domains":        {Href: branchURL + "/domains"},
				"stats":          {Href: branchURL + "/stats"},
				"events":         {Href: branchURL + "/events"},
				"synthesis-runs": {Href: branchURL + "/synthesis-runs"},
				"index-rebuilds": {Href: branchURL + "/index-rebuilds"},
				"mcp":            {Href: branchURL + "/mcp{?profile}", Templated: true},
				"repo":           {Href: b.Repo(repoName)},
			},
		}
		hal.WriteHAL(w, http.StatusOK, body)
	}
}
