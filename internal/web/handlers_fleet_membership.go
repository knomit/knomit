package web

import (
	"net/http"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// The fleet membership routes (repos.Manager's fleet_membership.go). Every
// one is mounted behind RequireLocalVia (router): it uses the fleet root key,
// or installs or removes this instance's trust material.

// handleEnrollFleet: POST /api/v1/fleet/enroll {agent_id, public_key}, the
// master issues a joiner's leaf and returns its bundle. 201.
func handleEnrollFleet(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AgentID   string `json:"agent_id"`
			PublicKey string `json:"public_key"`
		}
		if !decodeJSON(w, r, &req, 0) {
			return
		}
		out, err := m.EnrollFleetMember(r.Context(), req.AgentID, req.PublicKey)
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusCreated, out)
	}
}

// handleJoinFleet: POST /api/v1/fleet/join {url, auth_method, auth_token,
// bundle}, join the fleet at url with the bundle its master's enroll
// returned. 200, state registered, with the installed root's
// root_fingerprint; a refusal leaves nothing behind.
func handleJoinFleet(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL        string `json:"url"`
			AuthMethod string `json:"auth_method,omitempty"`
			AuthToken  string `json:"auth_token,omitempty"`
			Bundle     string `json:"bundle"`
		}
		if !decodeJSON(w, r, &req, 0) {
			return
		}
		if req.URL == "" || req.Bundle == "" {
			hal.WriteProblemWithExtra(w, http.StatusBadRequest, "URL and bundle required",
				"the body must carry a non-empty url and bundle", r.URL.Path, map[string]any{"code": "url_and_bundle_required"})
			return
		}
		st, err := m.JoinFleet(r.Context(), repos.FleetJoinSpec{URL: req.URL, AuthMethod: req.AuthMethod, AuthToken: req.AuthToken, Bundle: []byte(req.Bundle)})
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		view := fleetView(b, st.FleetStatus)
		view["root_fingerprint"] = st.RootFingerprint
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleLeaveFleet: POST /api/v1/fleet/leave, one push of the left record,
// then the fleet is removed here whatever the push did. 200, with pushed and
// push_error.
func handleLeaveFleet(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := m.LeaveFleet(r.Context())
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		view := fleetView(b, out.FleetStatus)
		view["pushed"] = out.Pushed
		if out.PushError != "" {
			view["push_error"] = out.PushError
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleRevokeFleet: POST /api/v1/fleet/revoke {agent_id, reason}, the master
// revokes a member's leaf.
func handleRevokeFleet(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AgentID string `json:"agent_id"`
			Reason  string `json:"reason,omitempty"`
		}
		if !decodeJSON(w, r, &req, 0) {
			return
		}
		out, err := m.RevokeFleetMember(r.Context(), req.AgentID, req.Reason)
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, out)
	}
}

// handleDecommissionFleet: POST /api/v1/fleet/decommission, the master ends
// the fleet here and deletes its root.
func handleDecommissionFleet(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := m.DecommissionFleet(r.Context())
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, fleetView(b, st))
	}
}

// handleGetFleetRoot: GET /api/v1/fleet/root, the files of <home>/fleet, the
// root key among them.
func handleGetFleetRoot(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := m.FleetRootFiles()
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		hal.WriteHAL(w, http.StatusOK, out)
	}
}

// handlePutFleetRoot: PUT /api/v1/fleet/root {files}, install another
// master's root files here; refused while a root key is here. 201.
func handlePutFleetRoot(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Files map[string]string `json:"files"`
		}
		if !decodeJSON(w, r, &req, 0) {
			return
		}
		out, err := m.InstallFleetRoot(req.Files)
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusCreated, out)
	}
}

// handleDeleteFleetRoot: DELETE /api/v1/fleet/root, stand down as master (the
// root files go, the membership stays); refused while this copy is the hub.
// 204.
func handleDeleteFleetRoot(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := m.StandDown(r.Context()); err != nil {
			writeFleetError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
