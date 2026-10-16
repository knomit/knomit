package web

import (
	"errors"
	"net/http"

	"knomit/internal/pki"
	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// The fleet membership routes (repos.Manager's fleet_membership.go). Every
// one is mounted behind RequireLocalVia (router): it uses the fleet root key,
// or installs or removes this instance's trust material.

// handleEnrollFleet: POST /api/v1/fleet/enroll {agent_id, public_key,
// address?}, the master issues a joiner's leaf and returns its bundle, which
// carries the fleet repository URL. 201.
func handleEnrollFleet(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AgentID   string `json:"agent_id"`
			PublicKey string `json:"public_key"`
			Address   string `json:"address,omitempty"`
		}
		if !decodeJSON(w, r, &req, 0) {
			return
		}
		out, err := m.EnrollFleetMember(r.Context(), req.AgentID, req.PublicKey, req.Address)
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusCreated, out)
	}
}

// handleJoinFleet: POST /api/v1/fleet/join {bundle, auth_method, auth_token},
// join the fleet at the URL the bundle (its master's enroll) names; the
// credential is the joiner's own, for a forge-hosted fleet only. 200, state
// registered, with the installed root's root_fingerprint; a refusal leaves
// nothing behind.
func handleJoinFleet(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeJoinRequest(w, r)
		if !ok {
			return
		}
		st, err := m.JoinFleet(r.Context(), repos.FleetJoinSpec{AuthMethod: req.AuthMethod, AuthToken: req.AuthToken, Bundle: []byte(req.Bundle)})
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, fleetView(b, r, st))
	}
}

// joinRequest is the body of POST /fleet/join and /fleet/join:check.
type joinRequest struct {
	AuthMethod string `json:"auth_method,omitempty"`
	AuthToken  string `json:"auth_token,omitempty"`
	Bundle     string `json:"bundle"`
}

// decodeJoinRequest reads a join body and runs its edge checks: a bundle is
// required (bundle_required), and auth_method must fit the URL the bundle
// names, as for every other origin (auth_invalid; cert is never an input).
// A bundle that does not parse is left to JoinFleet's bundle_invalid.
func decodeJoinRequest(w http.ResponseWriter, r *http.Request) (joinRequest, bool) {
	var req joinRequest
	if !decodeJSON(w, r, &req, 0) {
		return req, false
	}
	if req.Bundle == "" {
		hal.WriteProblemWithExtra(w, http.StatusBadRequest, "Bundle required",
			"the body must carry a non-empty bundle", r.URL.Path, map[string]any{"code": "bundle_required"})
		return req, false
	}
	if bundle, err := pki.SplitBundle([]byte(req.Bundle)); err == nil && bundle.FleetURL != "" {
		if verr := validateURLAuth(bundle.FleetURL, req.AuthMethod); verr != nil {
			hal.WriteProblemWithExtra(w, http.StatusUnprocessableEntity, "Invalid origin auth",
				verr.Error(), r.URL.Path, map[string]any{"code": "auth_invalid"})
			return req, false
		}
	}
	return req, true
}

// handleCheckFleetJoin: POST /api/v1/fleet/join:check {bundle, auth_method,
// auth_token}, the join's Check step: its pre-flights, then the bundle's URL
// reached with the bundle's certificate and judged against the bundle's
// root, installing nothing. 200 {url, hosting, host, root_fingerprint,
// agent_id, auth_required, detail}: the operator confirms the root
// fingerprint before the join installs it (#299).
func handleCheckFleetJoin(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeJoinRequest(w, r)
		if !ok {
			return
		}
		out, err := m.CheckFleetJoin(r.Context(), repos.FleetJoinSpec{AuthMethod: req.AuthMethod, AuthToken: req.AuthToken, Bundle: []byte(req.Bundle)})
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, out)
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
		view := fleetView(b, r, out.FleetStatus)
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
		hal.WriteHAL(w, http.StatusOK, fleetView(b, r, st))
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

// handleHostFleet: POST /api/v1/fleet/host {confirm_loss?}, "Host the fleet
// here": this instance's copy of the fleet repository drops its origin and
// becomes the hub (repos.Manager.HostFleetHere). A member is
// member_keeps_origin; a master with no mTLS listener or no external address
// is listener_required or addresses_required; a copy that already hosts the
// fleet is hub. A copy that follows another instance of the fleet whose main
// does not say the old master stood down is old_master_active unless
// confirm_loss is given.
func handleHostFleet(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ConfirmLoss bool `json:"confirm_loss"`
		}
		if !decodeOptionalJSON(w, r, &req, 0) {
			return
		}
		st, err := m.HostFleetHere(r.Context(), repos.FleetHostSpec{ConfirmLoss: req.ConfirmLoss})
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, fleetView(b, r, st))
	}
}

// followRequest is the body of POST /fleet/follow.
type followRequest struct {
	URL        string `json:"url"`
	AuthMethod string `json:"auth_method,omitempty"`
	AuthToken  string `json:"auth_token,omitempty"`
	User       string `json:"user,omitempty"`
	Password   string `json:"password,omitempty"`
}

// handleFollowFleet: POST /api/v1/fleet/follow {url}, "Point at the new
// host": this instance's copy of the fleet repository follows the fleet from
// url (repos.Manager.FollowFleetHost). The URL must be an instance of this
// fleet (not_this_fleet), one a join could clone (origin_invalid), and
// reachable (unreachable); the attach's own refusals map as PUT origin's do.
func handleFollowFleet(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req followRequest
		if !decodeJSON(w, r, &req, 0) {
			return
		}
		req.URL = trimOriginURL(req.URL)
		// The credential as PUT origin assembles it (basic: user:password).
		// An empty url is a credential change on a forge copy: the stored URL
		// stays, and repos refuses it (url_required) on any other copy.
		token := assembleAuthToken(req.AuthMethod, req.AuthToken, req.User, req.Password)
		if req.URL != "" {
			if verr := validateURLAuth(req.URL, req.AuthMethod); verr != nil {
				hal.WriteProblemWithExtra(w, http.StatusUnprocessableEntity, "Invalid origin", verr.Error(), r.URL.Path, map[string]any{"code": "auth_invalid"})
				return
			}
		}
		st, err := m.FollowFleetHost(r.Context(), repos.OriginSpec{URL: req.URL, AuthMethod: req.AuthMethod, AuthToken: token})
		var fe *repos.FleetError
		switch {
		case err == nil:
			// change says what ran: followed, moved, credential, or unchanged
			// (the stored forge and credential: nothing ran, nothing pushed).
			view := fleetView(b, r, st.FleetStatus)
			view["change"] = st.Change
			hal.WriteHAL(w, http.StatusOK, view)
		case errors.As(err, &fe):
			writeFleetError(w, r, err)
		case errors.Is(err, repos.ErrPeerCredential), errors.Is(err, repos.ErrCertNotAnInput), errors.Is(err, pki.ErrUnsupportedOriginScheme):
			hal.WriteProblemWithExtra(w, http.StatusUnprocessableEntity, "Invalid origin", err.Error(), r.URL.Path, map[string]any{"code": "auth_invalid"})
		case errors.Is(err, repos.ErrOriginUnreachable):
			hal.WriteProblemWithExtra(w, http.StatusBadGateway, "Unreachable", err.Error(), r.URL.Path, map[string]any{"code": "unreachable"})
		case errors.Is(err, repos.ErrOriginOntologyConflict):
			hal.WriteProblemWithExtra(w, http.StatusConflict, "Different knowledge base", err.Error(), r.URL.Path, map[string]any{"code": "not_this_fleet"})
		default:
			writeFleetError(w, r, err)
		}
	}
}

// handleFleetIndexRebuild: POST /api/v1/fleet/index-rebuild, the fleet
// repository's Rebuild (repos.Manager.RebuildFleetIndex): 201 with the job,
// 200 when an identical rebuild absorbed it. Its progress reads through the
// generic job route named in _links.job.
func handleFleetIndexRebuild(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rb, err := m.RebuildFleetIndex(r.Context())
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		status := http.StatusCreated
		if rb.Absorbed {
			status = http.StatusOK
		}
		hal.WriteHAL(w, status, map[string]any{
			"id": rb.JobID, "kind": "index-rebuild", "state": "running", "repo": rb.Repo, "branch": rb.Branch,
			"_links": hal.LinkMap{"job": {Href: b.Branch(rb.Repo, hal.Anchor{Branch: rb.Branch}) + "/index-rebuilds/" + rb.JobID}},
		})
	}
}

// handleDropFleetCommit: POST /api/v1/fleet/drop-commit {commit}, the way out
// when the judge of this instance's own commits holds the fleet's main back
// (GET /fleet last_error_main_held): the fleet repository's agent branch is
// rebuilt on main without that commit, then main is published
// (repos.Manager.DropFleetCommit). 200 with what was dropped and whether main
// moved; 409 not_registered or not_unpublished; 422 invalid_commit.
func handleDropFleetCommit(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Commit string `json:"commit"`
		}
		if !decodeJSON(w, r, &req, 0) {
			return
		}
		out, err := m.DropFleetCommit(r.Context(), req.Commit)
		if err != nil {
			writeFleetError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, out)
	}
}
