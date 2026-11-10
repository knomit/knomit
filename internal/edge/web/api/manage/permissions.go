package manage

import (
	"net/http"

	"knomit/internal/edge/web/httpx"
	"knomit/internal/foundation/authz"
)

// The route permission registry's resolvers for this package's
// body-dependent routes (internal/edge/web route_rules.go, unified authn/z
// P1). Each one decodes the body into its HANDLER'S OWN request struct with
// httpx.PeekJSON — the decoder DecodeJSON uses — and applies the same test
// the handler applies before its operator or admin gate, so the two cannot
// disagree about what a body says (case-insensitive field names, duplicate
// keys, an empty object). A body PeekJSON cannot decode resolves to the
// strongest permission.
//
// NOT YET ENFORCED, and they do not replace the handlers' own checks: the
// in-handler gates (httpx.Admits(operator|admin) here and in api/origins)
// must stay, rewritten at the flip (P4) as Policy.Has(repo:origin) and
// Policy.Has(secret:manage). They are the authority; these only choose the
// route-level permission. Some of those checks depend on stored state a
// resolver cannot see (a repo whose origin is linked to a stored secret).

// ResolveRepoCreate is POST /repos: an origin (any origin object, even {})
// needs repo:origin (the operator gate in handleHALReposCreate), an origin
// naming a stored secret secret:manage (admitSecretID's admin gate), anything
// else repo:manage.
func ResolveRepoCreate(r *http.Request) authz.Permission {
	var req createRepoRequest
	if !httpx.PeekJSON(r, &req, maxCreateBodyBytes) {
		return authz.SecretManage
	}
	switch {
	case req.Origin == nil:
		return authz.RepoManage
	case req.Origin.SecretID != "":
		return authz.SecretManage
	}
	return authz.RepoOrigin
}

// ResolveArchivedRestore is POST /archived/{id}/restore, with
// handleHALArchivedRestore's two tests: a stored secret needs secret:manage,
// a pasted credential (an auth method, or the token AssembleAuthToken builds)
// repo:origin, a plain restore repo:manage.
func ResolveArchivedRestore(r *http.Request) authz.Permission {
	var req restoreRequest
	if !httpx.PeekJSON(r, &req, 0) {
		return authz.SecretManage
	}
	switch {
	case req.SecretID != "":
		return authz.SecretManage
	case req.AuthMethod != "" || httpx.AssembleAuthToken(req.AuthMethod, req.Token, req.User, req.Password) != "":
		return authz.RepoOrigin
	}
	return authz.RepoManage
}

// ResolveProbeOrigin is POST /repos:probe-origin: secret_id needs
// secret:manage (admitSecretID), anything else repo:origin.
func ResolveProbeOrigin(r *http.Request) authz.Permission {
	var req probeOriginRequest
	if !httpx.PeekJSON(r, &req, 0) || req.SecretID != "" {
		return authz.SecretManage
	}
	return authz.RepoOrigin
}

// ResolveProbeInitialized is POST /repos:probe-initialized, as
// ResolveProbeOrigin.
func ResolveProbeInitialized(r *http.Request) authz.Permission {
	var req probeInitializedRequest
	if !httpx.PeekJSON(r, &req, maxCreateBodyBytes) || req.SecretID != "" {
		return authz.SecretManage
	}
	return authz.RepoOrigin
}

// ResolveOriginCredential is PUT /repos/{repo}/origin/credential: ANY
// secret_id — a stored secret's id, or null, which unlinks one — needs
// secret:manage (handleSetOriginCredential's admin gate after
// parseSecretIDField), anything else repo:origin.
func ResolveOriginCredential(r *http.Request) authz.Permission {
	var req setCredentialRequest
	if !httpx.PeekJSON(r, &req, 0) || len(req.SecretID) > 0 {
		return authz.SecretManage
	}
	return authz.RepoOrigin
}

// ResolveFleetCreate is POST /fleet/create: an origin naming a stored secret
// needs secret:manage, anything else fleet:manage.
func ResolveFleetCreate(r *http.Request) authz.Permission {
	var req createFleetRequest
	if !httpx.PeekJSON(r, &req, 0) || (req.Origin != nil && req.Origin.SecretID != "") {
		return authz.SecretManage
	}
	return authz.FleetManage
}

// ResolveFleetJoin is POST /fleet/join and /fleet/join:check (one request
// struct, decodeJoinRequest): secret_id needs secret:manage.
func ResolveFleetJoin(r *http.Request) authz.Permission {
	var req joinRequest
	if !httpx.PeekJSON(r, &req, 0) || req.SecretID != "" {
		return authz.SecretManage
	}
	return authz.FleetManage
}

// ResolveFleetFollow is POST /fleet/follow: secret_id needs secret:manage.
func ResolveFleetFollow(r *http.Request) authz.Permission {
	var req followRequest
	if !httpx.PeekJSON(r, &req, 0) || req.SecretID != "" {
		return authz.SecretManage
	}
	return authz.FleetManage
}
