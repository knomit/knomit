package web

import (
	"context"
	"net/http"
	"sort"
	"time"

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
	experimentExpiryDays int,
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
		hal.WriteHAL(w, http.StatusOK, branchRootBody(b, repoName, branch, info, ri, agentBranch, embeddingsEnabled, experimentExpiryDays))
	}
}

// branchRootBody renders a branch root. It is the ONE construction of this
// body: GET /repos/{repo}/branches/{branch} returns it, and the repo and lens
// resources embed it so a booting client reaches a known branch without a
// second round trip. Two surfaces that must agree agree by construction here,
// never by two renderings kept in step by hand.
//
// The branch NAME is an explicit parameter rather than read from ri, because
// the callers mean different branches and the difference is load-bearing: a
// CONTENT question uses ri.ReadBranch() (a subscription has no agent branch
// and would read from ""), a WRITE question uses ri.AgentBranch(). See
// kb/conventions/repos/subscription/read-branch-for-content/20c4c711.md.
//
// agentBranch is the SERVER's configured agent branch, which is what
// is_agent_branch has always compared against — not ri.AgentBranch(). Passing
// anything else here would make the embed disagree with the branch GET.
func branchRootBody(
	b hal.URLBuilder,
	repoName, branch string,
	info branchRootInfo,
	ri *repos.RepoInstance,
	agentBranch string,
	embeddingsEnabled bool,
	experimentExpiryDays int,
) map[string]any {
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
	return map[string]any{
		"name":               branch,
		"head":               info.Head,
		"index_commit":       info.IndexCommit,
		"embeddings_enabled": embeddingsEnabled,
		"is_agent_branch":    branch == agentBranch,
		"writable":           ri.WritableBranch(branch),
		// Present ONLY when this branch is a recorded experiment. The UI
		// keys its in-experiment marker and its since-fork filter on the
		// key's presence, and its read-only gate on `writable` above —
		// NOT on is_agent_branch, which is false for every experiment and
		// would gate a branch the user may legitimately write.
		"experiment":     branchExperiment(ri, branch, experimentExpiryDays),
		"ontology_error": ontologyErr,
		"index_state":    idxState, // "ready" | "indexing" | "error"
		"index_done":     idxDone,
		"index_total":    idxTotal,
		"index_percent":  indexPercent(idxState, idxDone, idxTotal), // 0–100; 100 when ready

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
}

// embedBranchRoot builds a branch root for embedding, or nil when it cannot be
// built. nil means the key is OMITTED: the embed is an optimisation, and a
// repo whose store is still opening must still answer its GET — the client
// falls back to the branch call. An error here is never the response's error.
func embedBranchRoot(
	ctx context.Context,
	b hal.URLBuilder,
	reader func(context.Context, *repos.RepoInstance, string) (branchRootInfo, error),
	repoName, branch string,
	ri *repos.RepoInstance,
	agentBranch string,
	embeddingsEnabled bool,
	experimentExpiryDays int,
) map[string]any {
	// An empty branch is "this repo has none" (a subscription has no agent
	// branch), not "unknown" — building a root for branch "" would invent a
	// resource. See kb/invariants/repos/subscription/flag-and-branch-paired.
	if ri == nil || reader == nil || branch == "" {
		return nil
	}
	info, err := reader(ctx, ri, branch)
	if err != nil {
		return nil
	}
	// An unopened store is NOT an error from defaultBranchRootReader: WithRead
	// simply never runs its callback and the zero branchRootInfo comes back
	// with a nil error. Embedding that would advertise a branch root whose
	// head is "" as though it were known. The embed exists to save a round
	// trip to a KNOWN branch, so with no head resolved there is nothing worth
	// embedding and the client should ask — which the omission tells it to do.
	if info.Head == "" {
		return nil
	}
	return branchRootBody(b, repoName, branch, info, ri, agentBranch, embeddingsEnabled, experimentExpiryDays)
}

// branchExperiment returns the experiment record for branch, or nil when the
// branch is not one. nil marshals as null and the client tests presence.
//
// fork_commit is what the "changed since the fork" fact filter anchors on, so
// it is part of the branch's own payload rather than something the UI has to
// fetch from the experiments collection before it can render a filter.
//
// expires_at is OMITTED when expiry is disabled. Absence means never; a
// computed far-future date would be a claim nobody made, and a null would be
// indistinguishable from "not yet calculated".
func branchExperiment(ri *repos.RepoInstance, branch string, expiryDays int) map[string]any {
	name, ok := store.ExperimentNameOf(branch)
	if !ok {
		return nil
	}
	var (
		exp   store.Experiment
		found bool
	)
	// A read failure is reported as "not an experiment" rather than failing
	// the branch GET: the rest of this body is still correct and useful, and
	// the writable flag above — which is what actually gates the UI — has
	// already been answered by the same classification.
	if err := ri.WithRead(func(svc *store.Service) {
		exp, found, _ = svc.Experiments().GetExperiment(context.Background(), name)
	}); err != nil || !found {
		return nil
	}
	out := map[string]any{
		"name":          exp.Name,
		"description":   exp.Description,
		"parent":        exp.Parent,
		"fork_commit":   exp.ForkCommit,
		"created_at":    exp.CreatedAt.UTC().Format(time.RFC3339),
		"last_activity": exp.LastActivityAt.UTC().Format(time.RFC3339),
	}
	if expiryDays > 0 {
		out["expires_at"] = exp.LastActivityAt.
			Add(time.Duration(expiryDays) * 24 * time.Hour).UTC().Format(time.RFC3339)
	}
	return out
}
