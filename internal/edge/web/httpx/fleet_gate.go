package httpx

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
)

// FleetRepoRefused refuses every MUTATION of the fleet repository through
// the generic /repos/{repo} routes, for every caller, the desktop app on the
// local socket included (user ruling 2026-10-16): the fleet repository is
// created, changed and removed only through the /fleet API, whose actions the
// Fleet page offers. Rename, archive, the repo PATCH, PUT and DELETE origin,
// the upstream, origin sessions, the pushed-branch merge, branch and fact
// writes, experiments, index and synthesis jobs: every one answers 409
// fleet_repository, and its handler does not run. That one check replaces
// the per-handler fleet arms those routes used to carry (R3.3's member-only
// refusals, the master's DELETE-origin path, the manual merge's re-judge).
//
// It is ONE gate over the whole subtree, by method, so a mutating route
// added later is covered without anyone remembering to. Reads (GET, HEAD,
// OPTIONS) pass, and so does DELETE on the FactSet resource (closing a
// FactSet frees session data, not repo data), and so does the MCP mount: MCP is a POST for reads too, and
// every MCP write on the fleet repository is refused by
// RepoInstance.WritableBranch. Any other repository, and a name that
// resolves to none (the handler's own 404 then answers), pass unchanged.
func FleetRepoRefused(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fleetRepoMutation(m, r) {
				text := fleetRepositoryRefusal
				if strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/index-rebuilds") {
					text = fleetRebuildRefusal
				}
				hal.WriteProblemWithExtra(w, http.StatusConflict, "Fleet repository", text,
					r.URL.Path, map[string]any{"code": "fleet_repository"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// fleetRepositoryRefusal is FleetRepoRefused's text: what to do instead, in
// the Fleet page's words.
const fleetRepositoryRefusal = "this is the fleet repository: it changes only through the Fleet page (Fleet → Settings: Move the fleet to a forge, Update access, Point at the new host, Host the fleet here, Index → Rebuild, Leave, Decommission), never through a repository's own actions"

// fleetRebuildRefusal is FleetRepoRefused's text for the generic Rebuild
// (POST …/index-rebuilds) on the fleet repository: the Fleet page has its own.
const fleetRebuildRefusal = "this is the fleet repository: rebuild its index from the Fleet page (Fleet → Settings → Index → Rebuild), never through a repository's own actions"

// fleetRepoMutation: r is a mutating request on this instance's fleet
// repository (not its MCP mount).
func fleetRepoMutation(m *repos.Manager, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	name := chi.URLParam(r, "repo")
	ri := m.Get(name)
	if ri == nil || !ri.IsFleet() {
		return false
	}
	if r.Method == http.MethodDelete && isRepoFactSetResource(r.URL.EscapedPath(), name) {
		return false
	}
	return !isRepoMCPMount(r.URL.EscapedPath(), name)
}

// isRepoFactSetResource: the escaped path is exactly
// …/repos/{name}/branches/{branch}/search/factset. Its DELETE closes the
// caller's own FactSet: that deletes transient session data (a row of the
// session DB and a cache entry), never repository data, so the fleet gate lets
// it through (user ruling 2026-11-01). Matched on the whole shape: the export
// below it, and a fact path that ends in search/factset, are not it.
func isRepoFactSetResource(escaped, name string) bool {
	marker := "/repos/" + url.PathEscape(name) + "/branches/"
	i := strings.Index(escaped, marker)
	if i < 0 {
		return false
	}
	parts := strings.Split(escaped[i+len(marker):], "/")
	return len(parts) == 3 && parts[0] != "" && parts[1] == "search" && parts[2] == "factset"
}

// isRepoMCPMount: the escaped path is exactly …/repos/{name}/branches/{branch}/mcp
// (a branch is one escaped segment). Matched on the whole shape, so a fact
// path that ends in "mcp" is not mistaken for the mount.
func isRepoMCPMount(escaped, name string) bool {
	marker := "/repos/" + url.PathEscape(name) + "/branches/"
	i := strings.Index(escaped, marker)
	if i < 0 {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(escaped[i+len(marker):], "/"), "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] == "mcp"
}
