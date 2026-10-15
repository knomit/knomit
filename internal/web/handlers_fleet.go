package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// fleetView is a FleetStatus as HAL.
func fleetView(b hal.URLBuilder, st repos.FleetStatus) map[string]any {
	raw, _ := json.Marshal(st)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	links := hal.LinkMap{"self": {Href: b.APIRoot() + "/fleet"}}
	if st.FleetRepo != "" {
		links["members"] = hal.Link{Href: b.APIRoot() + "/fleet/members"}
	}
	out["_links"] = links
	return out
}

// writeFleetError maps a refusal to problem+json with its machine code, and
// anything else to 500.
func writeFleetError(w http.ResponseWriter, r *http.Request, err error) {
	var fe *repos.FleetError
	if errors.As(err, &fe) {
		hal.WriteProblemWithExtra(w, fe.Status, http.StatusText(fe.Status), fe.Msg, r.URL.Path, map[string]any{"code": fe.Code})
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
		hal.WriteHAL(w, http.StatusOK, fleetView(b, st))
	}
}

type registerFleetRequest struct {
	URL        string `json:"url"`
	AuthMethod string `json:"auth_method,omitempty"`
	AuthToken  string `json:"auth_token,omitempty"`
}

// handlePutFleet: PUT /api/v1/fleet {url}, register (or refresh the record
// with the same fleet). 202 while the registration push is pending.
func handlePutFleet(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req registerFleetRequest
		if !decodeJSON(w, r, &req, 0) {
			return
		}
		if req.URL == "" {
			hal.WriteProblemWithExtra(w, http.StatusBadRequest, "URL required", "the body must carry a non-empty url", r.URL.Path, map[string]any{"code": "url_required"})
			return
		}
		st, err := m.RegisterFleet(r.Context(), req.URL, req.AuthMethod, req.AuthToken)
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		status := http.StatusOK
		if st.State != repos.FleetRegistered {
			status = http.StatusAccepted
		}
		hal.WriteHAL(w, status, fleetView(b, st))
	}
}

// handleDeleteFleet: DELETE /api/v1/fleet, unregister. 200 when the
// departure was pushed and the fleet unmounted; 202 while the push is being
// retried (state unregistering, last_error set).
func handleDeleteFleet(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := m.UnregisterFleet(r.Context())
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		status := http.StatusOK
		if st.State != repos.FleetStandalone {
			status = http.StatusAccepted
		}
		hal.WriteHAL(w, status, fleetView(b, st))
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
		if !decodeJSON(w, r, &req, 0) {
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
		hal.WriteHAL(w, createdFleetStatus(st), fleetView(b, st))
	}
}

// fleetMemberRow is one member as GET /api/v1/fleet/members shows it: every
// field as the record has it (someone else's odd values are shown, never
// used), addresses always an array and capabilities always an object.
func fleetMemberRow(mem store.FleetMember) map[string]any {
	f := repos.MemberFieldsOf(mem.Member)
	return map[string]any{
		"agent": mem.Agent, "state": mem.State, "host": mem.Host, "branch": mem.Branch, "path": mem.Path,
		"addresses": f.Addresses, "git": f.Git, "capabilities": f.Capabilities,
	}
}

// fleetMemberRows is one row per record; a member whose branch (the record's
// `branch`) the fleet's master last refused carries it as last_refusal.
func fleetMemberRows(ms []store.FleetMember, refused map[string]repos.PeerRefusal) []map[string]any {
	rows := make([]map[string]any, 0, len(ms))
	for _, mem := range ms {
		row := fleetMemberRow(mem)
		if r, ok := refused[mem.Branch]; ok && mem.Branch != "" {
			row["last_refusal"] = r
		}
		rows = append(rows, row)
	}
	return rows
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
