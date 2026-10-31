// Package kb serves the knowledge-base surface: a branch's facts (read,
// create, update, delete), topics, changes, commits and the reads anchored at
// a commit, search, activity, completions, domains, motifs, stats, events and
// triggers; and the federated reads of a lens over its members (facts,
// search, completions, motifs, stats, topics). Its only export is Mount; the
// handlers are private to it.
package kb

import (
	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/store/model"
)

// Mount registers the KB routes into two subrouters the caller owns and has
// already put the middleware on. The caller builds the route skeleton, and
// every middleware it carries, because the lifecycle routes (api/manage), the
// job routes (api/jobs) and the MCP mounts share these same subtrees and must
// keep their nesting.
//
//   - branch is the /repos/{repo}/branches/{branch} subtree INSIDE the fleet
//     gate, RepoMiddleware and httpx.BranchMiddleware: every handler reads the
//     repo with repos.RepoFromContext and the branch with
//     httpx.BranchFromContext.
//   - lens is the /lenses/{lens} subtree INSIDE LensMiddleware: every handler
//     reads the resolved lens from the request context. The lens CRUD
//     (api/manage) is registered outside that group, on its parent.
//
// ontologyRoot is the KB root folder inside each repo (config OntologyRoot),
// which the fact writes and the topic and commit reads resolve paths against.
// emb is the server's query embedder for the repo and lens search and the
// lens facts read with text. When it is nil those handlers do not embed the
// query themselves and leave it to each repo's store, as before the move.
// mgr is the server's repo registry: the facts/*/materialize sub-resource
// follows cross-KB refs into any repo registered in it.
func Mount(branch, lens chi.Router, ontologyRoot string, emb model.Embedder, mgr *repos.Manager) {
	b := hal.URLBuilder{Base: httpx.APIBase}

	branch.Get("/facts", handleHALFactsCollection(b))
	branch.Post("/facts", handleFactCreate(b, ontologyRoot))
	branch.Get("/facts/*", handleHALFact(b, mgr))
	branch.Put("/facts/*", handleFactUpdate(b, ontologyRoot))
	branch.Delete("/facts/*", handleFactDelete(b))

	branch.Get("/topics", handleTopics(b, ontologyRoot))
	branch.Get("/topics/*", handleTopicNode(b, ontologyRoot))

	branch.Get("/changes", handleHALChanges(b))
	branch.Get("/commits", handleHALCommitsList(b, ontologyRoot))
	branch.Get("/commits/{sha}", handleHALCommitDetail(b, ontologyRoot))
	branch.Get("/commits/{sha}/facts/*", handleCommitAnchoredFact(b, mgr))
	branch.Get("/commits/{sha}/topics", handleCommitAnchoredTopicNode())
	branch.Get("/commits/{sha}/topics/*", handleCommitAnchoredTopicNode())

	branch.Get("/search", handleSearch(b, emb))
	branch.Get(factsetRouteSuffix, handleSearchFactSet(emb, mgr))
	branch.Get(factsetRouteSuffix+factsetExportSuffix, handleSearchFactSetExport(mgr))
	branch.Get("/activity", handleHALActivity(b))
	branch.Get("/completions", handleHALCompletions(b))
	branch.Get("/domains", handleHALDomains(b))
	branch.Get("/domains/{name}", handleHALDomainFacts(b))
	branch.Get("/motifs", handleHALMotifs(b))
	branch.Get("/motifs/{key}", handleHALMotifCluster(b))
	branch.Get("/stats", handleHALStats(b))
	branch.Get("/events", handleHALEvents())
	branch.Get("/triggers", handleHALTriggers(b))

	lens.Get("/facts", handleHALLensFacts(emb))
	lens.Get("/facts/*", handleHALLensFact(b, mgr))
	lens.Get("/search", handleHALLensSearch(emb))
	lens.Get("/completions", handleHALLensCompletions())
	lens.Get("/motifs", handleHALLensMotifs(b))
	lens.Get("/motifs/{key}", handleHALLensMotifCluster(b))
	lens.Get("/stats", handleHALLensStats())
	lens.Get("/topics", handleHALLensTopics(ontologyRoot))
	lens.Get("/topics/*", handleHALLensTopics(ontologyRoot))
}
