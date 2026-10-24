package web

import (
	"encoding/json"
	"errors"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
)

// fleetView is a FleetStatus as HAL, with `local`: the request came in over
// the local authenticated listener (httpx.IsLocalVia, the test httpx.RequireLocalVia
// makes), so the fleet's mutations are reachable from where the caller runs.
// host.listener (the [tls].addr bind) is shown only to a local caller; any
// other principal the edge admits sees host.listener_on, whether there is
// one, and not where it is bound.
func fleetView(b hal.URLBuilder, r *http.Request, st repos.FleetStatus) map[string]any {
	local := httpx.IsLocalVia(r)
	raw, _ := json.Marshal(st)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if host, ok := out["host"].(map[string]any); ok && !local {
		delete(host, "listener")
	}
	out["local"] = local
	links := hal.LinkMap{"self": {Href: b.APIRoot() + "/fleet"}}
	if st.FleetRepo != "" {
		links["members"] = hal.Link{Href: b.APIRoot() + "/fleet/members"}
		links["activity"] = hal.Link{Href: b.APIRoot() + "/fleet/activity"}
	}
	out["_links"] = links
	return out
}

// writeFleetError maps a refusal to problem+json with its machine code, and
// anything else to 500.
func writeFleetError(w http.ResponseWriter, r *http.Request, err error) {
	var fe *repos.FleetError
	if errors.As(err, &fe) {
		extra := map[string]any{"code": fe.Code}
		if len(fe.Choices) > 0 {
			extra["choices"] = fe.Choices
		}
		if len(fe.LeaveOut) > 0 {
			extra["leave_out"] = fe.LeaveOut
		}
		hal.WriteProblemWithExtra(w, fe.Status, http.StatusText(fe.Status), fe.Msg, r.URL.Path, extra)
		return
	}
	// The fleet repository's lifecycle refused (an origin change while its
	// index job runs). No cancel-index link: that is a generic repo route,
	// refused for the fleet repository; the job ends on its own.
	if errors.Is(err, repos.ErrIndexing) {
		hal.WriteProblemWithExtra(w, http.StatusConflict, "Fleet repository is indexing",
			"the fleet repository is indexing: wait for it to finish, then try again from Fleet → Settings", r.URL.Path,
			map[string]any{"code": "indexing"})
		return
	}
	if httpx.IsLifecycleRefusal(err) {
		httpx.WriteLifecycleProblem(w, r, hal.URLBuilder{}, "", err)
		return
	}
	hal.WriteProblem(w, http.StatusInternalServerError, "Fleet operation failed", err.Error(), r.URL.Path)
}

// handleGetFleet: GET /api/v1/fleet, the membership state machine.
func handleGetFleet(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := m.FleetStatus(r.Context())
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, fleetView(b, r, st))
	}
}

type createFleetRequest struct {
	Name   string `json:"name,omitempty"`
	Origin *struct {
		URL        string `json:"url"`
		Branch     string `json:"branch,omitempty"`
		AuthMethod string `json:"auth_method,omitempty"`
		AuthToken  string `json:"auth_token,omitempty"`
	} `json:"origin,omitempty"`
}

// createdFleetStatus is POST /fleet/create's success status: 201 only when
// the fleet's main holds this instance's active record (state registered AND
// record_state active, read at local main), 202 otherwise: a forge publish
// that failed, or a main that has not followed the agent branch yet.
func createdFleetStatus(st repos.FleetStatus) int {
	if st.State == repos.FleetRegistered && st.RecordState == fact.MemberActive {
		return http.StatusCreated
	}
	return http.StatusAccepted
}

// handleCreateFleet: POST /api/v1/fleet/create, found a fleet with a fresh
// root, this instance its master and first member (Manager.CreateFleet,
// which runs detached from the request: a client that hangs up does not cut
// it short). The status is createdFleetStatus's. Local listener only
// (router).
func handleCreateFleet(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createFleetRequest
		if !httpx.DecodeJSON(w, r, &req, 0) {
			return
		}
		spec := repos.FleetCreateSpec{Name: req.Name}
		if req.Origin != nil {
			spec.Origin = &repos.OriginSpec{URL: req.Origin.URL, Branch: req.Origin.Branch, AuthMethod: req.Origin.AuthMethod, AuthToken: req.Origin.AuthToken}
		}
		st, err := m.CreateFleet(r.Context(), spec)
		if err != nil {
			var fe *repos.FleetError
			if errors.As(err, &fe) {
				writeFleetError(w, r, err)
				return
			}
			// The repo create's own refusals (name, origin, reachability).
			status, title := createErrStatus(err)
			hal.WriteProblem(w, status, title, err.Error(), r.URL.Path)
			return
		}
		hal.WriteHAL(w, createdFleetStatus(st), fleetView(b, r, st))
	}
}

// fleetMemberRow is one member as GET /api/v1/fleet/members shows it: every
// field as the record has it (someone else's odd values are shown, never
// used) except state, which is "revoked" when the CRL at the same main lists
// the member's leaf, and "enrolled" for an id with a leaf on main and no
// record (repos.Manager.FleetMembers derives both); addresses always an array
// and capabilities always an object; since when the walk found it;
// revoke_due (only when true) for a left record whose leaf the master's leave
// duty revokes.
func fleetMemberRow(mem repos.FleetMemberRow) map[string]any {
	f := repos.MemberFieldsOf(mem.Member.Member)
	row := map[string]any{
		"agent": mem.Agent, "state": mem.State, "host": mem.Host, "branch": mem.Branch, "path": mem.Path,
		"addresses": f.Addresses, "git": f.Git, "capabilities": f.Capabilities,
	}
	if mem.Since != nil {
		row["since"] = mem.Since
	}
	if mem.RevokeDue {
		row["revoke_due"] = true
	}
	return row
}

// fleetMemberRows is one row per record or enrolled id; a member whose branch
// (the record's `branch`) the fleet's master last refused carries it as
// last_refusal. An enrolled id has no record and so no branch: it carries the
// newest refusal whose refused tip its agent authored (PeerRefusal.Agent, the
// commit's author address, as the judge reads it; nothing is parsed out of a
// branch name), which is a first join refused in flight.
func fleetMemberRows(ms []repos.FleetMemberRow, refused map[string]repos.PeerRefusal) []map[string]any {
	byAgent := map[string]repos.PeerRefusal{}
	for _, r := range refused {
		if cur, ok := byAgent[r.Agent]; r.Agent != "" && (!ok || r.At.After(cur.At)) {
			byAgent[r.Agent] = r
		}
	}
	rows := make([]map[string]any, 0, len(ms))
	for _, mem := range ms {
		row := fleetMemberRow(mem)
		if r, ok := refused[mem.Branch]; ok && mem.Branch != "" {
			row["last_refusal"] = r
		} else if r, ok := byAgent[mem.Agent]; ok && mem.Branch == "" && mem.State == repos.MemberEnrolled {
			row["last_refusal"] = r
		}
		rows = append(rows, row)
	}
	return rows
}

// handleFleetActivity: GET /api/v1/fleet/activity?limit=N, the fleet's newest
// events (repos.Manager.FleetActivity), newest first. limit defaults to and
// is capped at repos.MaxFleetActivity; one that is not a positive integer is
// 400. Read-only and open to any principal the edge admits, like members; a
// page polls it.
func handleFleetActivity(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := repos.MaxFleetActivity
		if s := r.URL.Query().Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid limit", "limit must be a positive integer", r.URL.Path)
				return
			}
			limit = min(n, repos.MaxFleetActivity)
		}
		events, err := m.FleetActivity(r.Context(), limit)
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		if events == nil {
			events = []repos.FleetEvent{}
		}
		hal.WriteHAL(w, http.StatusOK, map[string]any{
			"events": events,
			"_links": hal.LinkMap{"self": {Href: b.APIRoot() + "/fleet/activity"}},
		})
	}
}

// handleFleetMembers: GET /api/v1/fleet/members, the member records at the
// fleet repository's main.
func handleFleetMembers(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ms, err := m.FleetMembers(r.Context())
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		rows := fleetMemberRows(ms, m.FleetRefusals())
		hal.WriteHAL(w, http.StatusOK, map[string]any{
			"members": rows,
			"_links":  hal.LinkMap{"self": {Href: b.APIRoot() + "/fleet/members"}},
		})
	}
}

// fleetRepoRefused refuses every MUTATION of the fleet repository through
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
// OPTIONS) pass, and so does the MCP mount: MCP is a POST for reads too, and
// every MCP write on the fleet repository is refused by
// RepoInstance.WritableBranch. Any other repository, and a name that
// resolves to none (the handler's own 404 then answers), pass unchanged.
func fleetRepoRefused(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fleetRepoMutation(m, r) {
				text := FleetRepositoryRefusal
				if strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/index-rebuilds") {
					text = FleetRebuildRefusal
				}
				hal.WriteProblemWithExtra(w, http.StatusConflict, "Fleet repository", text,
					r.URL.Path, map[string]any{"code": "fleet_repository"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// FleetRepositoryRefusal is fleetRepoRefused's text: what to do instead, in
// the Fleet page's words.
const FleetRepositoryRefusal = "this is the fleet repository: it changes only through the Fleet page (Fleet → Settings: Move the fleet to a forge, Update access, Point at the new host, Host the fleet here, Index → Rebuild, Leave, Decommission), never through a repository's own actions"

// FleetRebuildRefusal is fleetRepoRefused's text for the generic Rebuild
// (POST …/index-rebuilds) on the fleet repository: the Fleet page has its own.
const FleetRebuildRefusal = "this is the fleet repository: rebuild its index from the Fleet page (Fleet → Settings → Index → Rebuild), never through a repository's own actions"

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
	return !isRepoMCPMount(r.URL.EscapedPath(), name)
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
