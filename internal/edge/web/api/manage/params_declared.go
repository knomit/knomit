package manage

import "knomit/internal/edge/web/httpx"

// The management API's GET routes' declared query parameters (user ruling
// 2026-11-02: the REST rule #718 and #720 hold on every knowledge-base route
// holds here too). Each handler calls RefuseUnknown first, so a parameter not
// in its set, or a malformed query string, is a 400 "Invalid parameter".
// Each set is exactly the route's openapi.yaml query parameters;
// TestManageRouteParams_MatchOpenAPI compares the two. To add a parameter to
// a route, add it here AND to the spec in the same change.
var (
	// GET /repo-events (a stream; reconnection resumes by the Last-Event-ID
	// header, not a parameter)
	repoEventsParams = httpx.DeclareParams()
	// GET /fleet
	fleetParams = httpx.DeclareParams()
	// GET /fleet/members
	fleetMembersParams = httpx.DeclareParams()
	// GET /fleet/activity
	fleetActivityParams = httpx.DeclareParams("limit")
	// GET /fleet/root (local listener only)
	fleetRootParams = httpx.DeclareParams()
	// GET /archived
	archivedParams = httpx.DeclareParams()
	// GET /archived/{id}
	archivedItemParams = httpx.DeclareParams()
	// GET /repos
	reposParams = httpx.DeclareParams()
	// GET /repo-creates
	repoCreatesParams = httpx.DeclareParams()
	// GET /repo-creates/{id}
	repoCreateParams = httpx.DeclareParams()
	// GET /ontologies/presets
	ontologyPresetsParams = httpx.DeclareParams()
	// GET /ontologies/presets/{name}
	ontologyPresetParams = httpx.DeclareParams()
	// GET /ontologies/schema
	ontologySchemaParams = httpx.DeclareParams()
	// GET /templates
	templatesParams = httpx.DeclareParams()
	// GET /repos/{repo}
	repoParams = httpx.DeclareParams()
	// GET /repos/{repo}/templates
	repoTemplatesParams = httpx.DeclareParams()
	// GET /repos/{repo}/origin
	originParams = httpx.DeclareParams()
	// GET /repos/{repo}/branches
	branchesParams = httpx.DeclareParams()
	// GET /repos/{repo}/pushed-branches
	pushedBranchesParams = httpx.DeclareParams()
	// GET /repos/{repo}/experiments
	experimentsParams = httpx.DeclareParams()
	// GET /repos/{repo}/branches/{branch}
	branchParams = httpx.DeclareParams()
	// GET /lenses
	lensesParams = httpx.DeclareParams()
	// GET /lenses/{lens}
	lensParams = httpx.DeclareParams()
)
