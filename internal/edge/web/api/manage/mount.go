// Package manage serves the repo, lens and fleet lifecycle surface: fleet
// membership, the archive, repo create / read / rename / patch, the origin,
// templates, ontologies, branches, pushed branches, experiments, index
// cancel, the repo-event stream, and the lens collection and its CRUD. Its
// only export is Mount; the handlers are private to it.
package manage

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
)

// Mount registers the lifecycle routes into five subrouters the caller owns
// and has already put the middleware on. The caller builds the route
// skeleton, and every middleware it carries, because the KB routes and the
// MCP mounts share these same subtrees and must keep their nesting.
//
//   - top is the API root: fleet, archived, the repo collection, the repo
//     create jobs, the probes, ontologies, templates, the repo-event stream
//     and the lens collection. None of them is wrapped by RepoMiddleware.
//   - repo is the /repos/{repo} subtree INSIDE the fleet gate and the
//     RepoMiddleware group: every handler reads the repo with
//     repos.RepoFromContext. Archive (the one route that resolves {repo}
//     itself) is registered on repoOuter instead, see below.
//   - branch is the /repos/{repo}/branches/{branch} subtree INSIDE
//     httpx.BranchMiddleware.
//   - lens is the /lenses/{lens} subtree OUTSIDE LensMiddleware: the lens
//     CRUD resolves {lens} through the registry itself.
//
// repoOuter is /repos/{repo} BEFORE the RepoMiddleware group: Archive
// resolves through m.Archive, not m.Get, and attributes its own errors, so
// the middleware's generic 404 must not wrap it.
//
// rm is the repo manager every handler acts through. agentBranch is the
// instance's agent branch, embeddingsEnabled whether the instance embeds, and
// expiryDays [experiments].expiry_days; the repo, branch and lens reads
// report them. operator is the gate that admits the host's operator only,
// which the pushed-branch merge and the origin writes sit behind; the caller
// builds it from its grants so this package does not know the auth model.
func Mount(
	top, repoOuter, repo, branch, lens chi.Router,
	rm *repos.Manager,
	agentBranch string,
	embeddingsEnabled bool,
	expiryDays int,
	operator func(http.Handler) http.Handler,
) {
	b := hal.URLBuilder{Base: httpx.APIBase}

	// Server-wide REPO-LEVEL event stream. A TOP-LEVEL collection, deliberately not
	// under /repos/: see the no-static-children rule on /repo-creates below.
	// It is about every repo rather than any one, and the UI holds exactly one.
	top.Get("/repo-events", handleRepoEvents(rm))
	// F09 fleet membership: a TOP-LEVEL resource (the instance's state, not a
	// repo's). The three reads are open to any principal the edge admits.
	top.Get("/fleet", handleGetFleet(b, rm))
	top.Get("/fleet/members", handleFleetMembers(b, rm))
	top.Get("/fleet/activity", handleFleetActivity(b, rm))
	// Every fleet operation uses the fleet ROOT KEY or installs or removes
	// this instance's trust material, so each is served to the local
	// authenticated listener only (httpx.RequireLocalVia: the unix socket or named
	// pipe) — never to a peer over mTLS, and never to a browser page on
	// loopback TCP (issue #521 opens them remotely later, under one rule).
	top.Group(func(r chi.Router) {
		r.Use(httpx.RequireLocalVia)
		r.Post("/fleet/create", handleCreateFleet(b, rm))
		r.Post("/fleet/enroll", handleEnrollFleet(rm))
		r.Post("/fleet/join", handleJoinFleet(b, rm))
		r.Post("/fleet/join:check", handleCheckFleetJoin(rm))
		r.Post("/fleet/leave", handleLeaveFleet(b, rm))
		r.Post("/fleet/revoke", handleRevokeFleet(rm))
		r.Post("/fleet/decommission", handleDecommissionFleet(b, rm))
		r.Get("/fleet/root", handleGetFleetRoot(rm))
		r.Put("/fleet/root", handlePutFleetRoot(rm))
		r.Delete("/fleet/root", handleDeleteFleetRoot(rm))
		r.Post("/fleet/host", handleHostFleet(b, rm))
		r.Post("/fleet/follow", handleFollowFleet(b, rm))
		r.Post("/fleet/index-rebuild", handleFleetIndexRebuild(b, rm))
		r.Post("/fleet/drop-commit", handleDropFleetCommit(rm))
	})
	top.Get("/archived", handleHALArchived(b, rm))
	top.Get("/archived/{id}", handleHALArchivedItem(b, rm))
	top.Post("/archived/{id}/restore", handleHALArchivedRestore(b, rm))
	top.Delete("/archived/{id}", handleHALArchivedPurge(rm))

	// Repo collection. These carry no {repo} segment, so they resolve their
	// own arguments and are never wrapped by RepoMiddleware.
	//
	// By convention, routes with a ':' action suffix (e.g. a future
	// "/repos:rehydrate") are registered up here, before "/repos/{repo}" —
	// though chi's radix tree does not actually depend on that order. It
	// treats only "{...}" and "*" as special, so such a route diverges from
	// "/repos/..." at the ':' and never enters the {repo} param edge either
	// way.
	top.Get("/repos", handleHALRepos(b, rm))
	// A create that names an origin chooses one, so it needs the operator
	// like PUT /origin; the handler applies the gate once it has read the
	// body (a create without an origin stays a `write`).
	top.Post("/repos", handleHALReposCreate(b, rm, operator))

	// The poll target for the 202 that POST /repos answers with. A SIBLING
	// collection rather than "/repos/creates/{id}", and the precedent for a
	// rule this router now holds to: NO STATIC ROUTE SITS BESIDE A {param}
	// WHOSE VALUE A USER CAN CAUSE TO EXIST.
	//
	// Such a segment is also a legal value for that param, so it shadows the
	// thing named that. SIX subtrees qualify today, and the prohibition covers
	// all of them:
	//
	//   /repos                                    {repo}
	//   /lenses                                   {lens}
	//   /repos/{repo}/branches                    {branch}
	//   /repos/{repo}/branches/{branch}/domains   {name}
	//   /repos/{repo}/branches/{branch}/motifs    {key}
	//   /lenses/{lens}/motifs                     {key}
	//
	// Repo and lens names share one validator (repos.IsValidName); branch names
	// are chosen too; domain tags and motif keys are AUTHORED — someone writing
	// `domain: [stats]` in a fact's frontmatter creates one, so /domains/stats
	// would shadow it. The test is "can a user cause a value with that name to
	// exist", not "does a user own it": ownership tracks how bad a collision
	// would be, not whether it can happen.
	//
	// The two /motifs entries are DISTINCT parents, not one — a lens-scoped and
	// a branch-scoped view, each shadowable in its own scope. Counting by last
	// segment gives five and silently drops one.
	//
	// /ontologies/presets/{name} is deliberately NOT in the set: those are our
	// shipped preset names, which no user adds to. It shares the {name} spelling
	// with /domains/{name} and has the opposite answer, which is why the test
	// classifies by parent prefix rather than by param name.
	// The cost was measured against the vendored chi rather than assumed — an
	// earlier version of this comment blamed the absence of backtracking and
	// was wrong, in a way that mispredicted which static routes are safe:
	//
	//   GET    /repos/events           -> the static route   <- shadowed
	//   DELETE /repos/events           -> {repo}             <- falls through
	//   GET    /repos/events/branches  -> {repo}             <- falls through
	//   GET    /repos/creates          -> {repo}             <- works
	//   GET    /repos/creates/branches -> creates/{id}       <- BROKEN
	//
	// chi DOES backtrack to the {repo} param when the static edge has no
	// handler for the method or no child for the next segment. So a static
	// LEAF costs exactly one method+path, while a static segment with a PARAM
	// CHILD costs the whole subtree below it, because the greedy {id} matches
	// "branches" and wins. Neither is diagnosable by the repo's owner, and the
	// difference is invisible from "static beats param" alone — which is why
	// the rule is the blunt one rather than a judgement per route.
	//
	// TestRouter_NoStaticSiblingsOfUserNamedParams enforces it, deriving the
	// guarded prefixes from the route table rather than from a list here.
	top.Get("/repo-creates/{id}", handleHALRepoCreateStatus(b, rm))
	// The collection: a client that lost the id from its 202 finds its create
	// here. DELETE forgets a FINISHED job so a failed row can be dismissed
	// without waiting out CreateJobTTL.
	top.Get("/repo-creates", handleHALRepoCreates(b, rm))
	top.Delete("/repo-creates/{id}", handleHALRepoCreateDismiss(rm))
	// Cancel is a POST on a colon-suffixed sub-resource, the same spelling as
	// /repos:probe-origin: it is an ACTION on the job, not a deletion of its
	// row, and the two must stay distinct because dismiss refuses a running
	// job while cancel is exactly for one.
	top.Post("/repo-creates/{id}:cancel", handleHALRepoCreateCancel(b, rm))

	// Probes an origin before create, so the wizard can classify it (has refs
	// / empty / unreachable) instead of asking the user to declare that up
	// front. Collection-level for the same reason as the ontology block below:
	// it runs BEFORE any repo exists. Both probes are the operator's, like the
	// origin-session wizard: they read a remote the caller names (refs, and
	// for :probe-initialized a tip and its ontology).
	top.With(operator).Post("/repos:probe-origin", handleReposProbeOrigin(rm))

	// The second, per-BRANCH half of that classification: does the chosen
	// branch already hold a knomit knowledge base? Separate from probe-origin
	// because the answer differs per branch and the branch is not known when
	// that probe runs — and because this one actually transfers a commit
	// (shallow, single-branch, discarded) rather than only listing refs.
	top.With(operator).Post("/repos:probe-initialized", handleReposProbeInitialized(rm))

	// Ontology endpoints. Collection-level and repo-independent: the create
	// wizard calls them BEFORE any repo exists, so they cannot live under
	// /repos/{repo}. The ':' action suffix follows the convention noted above.
	top.Post("/ontologies:validate", handleOntologyValidate())
	top.Get("/ontologies/presets", handleOntologyPresets())
	top.Get("/ontologies/presets/{name}", handleOntologyPresetYAML())
	top.Get("/ontologies/schema", handleOntologySchema())

	// Templates (F24): the create wizard lists them BEFORE any repo exists,
	// so the all-repos listing is collection-level like the ontology block.
	top.Get("/templates", handleTemplates(rm))

	// Archive deliberately sits OUTSIDE the RepoMiddleware group: it resolves
	// through m.Archive, not m.Get, and archiveErrStatus attributes its own
	// not-found/conflict errors. Wrapping it would replace that attribution
	// with a generic middleware 404. Registered with the "/" pattern inside
	// the Route rather than flat alongside it, which would risk a chi Mount
	// conflict.
	repoOuter.Delete("/", handleHALRepoArchive(b, rm))

	// Everything below resolves {repo} exactly once, in the middleware.
	// Handlers read the instance with repos.RepoFromContext.
	repo.Get("/", handleHALRepo(b, agentBranch, embeddingsEnabled, expiryDays))
	repo.Patch("/", handleHALRepoPatch(b))

	// Inside the middleware group, unlike Archive: {repo} must exist
	// for a rename, so the middleware's 404 is the right answer, and
	// every error this handler raises is about the NEW name, which the
	// middleware never sees.
	repo.Post("/rename", handleHALRepoRename(b, rm))

	// The templates this repo holds (F24), for a script that knows
	// the source.
	repo.Get("/templates", handleRepoTemplates(rm))

	// Setting, re-pointing or removing the origin is the host's operator
	// acting on the host, never a repo-scoped writer: an origin decides
	// which knowledge base this repo's consensus branch holds and where its
	// agent branch is pushed, so a caller confined to this repo could
	// otherwise import another knowledge base into it and read it. writeGate
	// still adds `write`. The local principals (socket, pipe, loopback per
	// [auth].loopback_default) hold `operator` by default.
	repo.Get("/origin", handleHALGetOrigin(b))
	repo.With(operator).Put("/origin", handleHALSetOrigin(b, rm))
	repo.With(operator).Patch("/origin/upstream", handleHALSetOriginUpstream(b, rm))
	repo.With(operator).Delete("/origin", handleHALDeleteOrigin(b, rm))

	// Cancels the repo's index job: an ACTION on the repo, a POST on a
	// colon-suffixed sub-resource like /repo-creates/{id}:cancel. It is
	// the way out of the 409 "Repo is indexing" that Attach, Detach and
	// Swap answer while the index job runs.
	repo.Post("/index:cancel", handleCancelIndex(b, rm))

	// Branch list carries no {branch} segment, so it stays outside
	// the httpx.BranchMiddleware subtree.
	repo.Get("/branches", handleHALBranches(b))

	// Branches a peer pushed to this host (F11), and what merging each
	// would bring. The merge itself is per branch, below.
	repo.Get("/pushed-branches", handleHALPushedBranches(b))

	// Experiments hang off the REPO, not off a branch: an experiment
	// IS a branch, so nesting it under /branches/{branch} would make
	// its URL depend on which branch the caller happened to be on.
	// `{name}` is the bare experiment name — the `exp/` prefix lives
	// in the branch namespace and would need escaping here.
	repo.Get("/experiments", handleHALExperiments(b, expiryDays))
	repo.Post("/experiments", handleExperimentOpen(b, expiryDays))
	repo.Route("/experiments/{name}", func(r chi.Router) {
		// DELETE is rollback: the destructive verb for the
		// destructive action, so a client that knows REST needs no
		// table to guess it. The named actions follow the ':' action
		// convention's spirit as path suffixes because they are
		// transitions, not resources.
		r.Delete("/", handleExperimentAction(b, "rollback", expiryDays))
		r.Post("/commit", handleExperimentAction(b, "commit", expiryDays))
		r.Post("/rollback", handleExperimentAction(b, "rollback", expiryDays))
		r.Post("/sync", handleExperimentAction(b, "sync", expiryDays))
	})

	branch.Get("/", handleHALBranch(b, agentBranch, embeddingsEnabled, expiryDays))

	// Merge THIS pushed branch into the repo's agent branch (F11 UI
	// merge). The first route that enforces `operator`: the host's
	// operator, never a peer (a peer's certificate holds read and
	// push:own only). writeGate adds `write` as for every mutation.
	branch.With(operator).Post("/merge", handlePushedMerge())

	// Lens collection: no {lens} segment, so unwrapped.
	top.Get("/lenses", handleHALLenses(b, rm))
	top.Post("/lenses", handleHALLensesCreate(b, rm))

	// The lens CRUD quartet — including rename — resolves through the
	// registry directly and reports its own errors, so it stays outside
	// the binding group.
	lens.Get("/", handleHALLens(b, rm, agentBranch, embeddingsEnabled, expiryDays))
	lens.Patch("/", handleHALLensPatch(b, rm))
	lens.Delete("/", handleHALLensDelete(rm))
	lens.Post("/rename", handleHALLensRename(b, rm))
}
