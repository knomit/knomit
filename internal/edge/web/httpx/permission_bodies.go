package httpx

import (
	"encoding/json"
	"net/http"

	"knomit/internal/foundation/authz"
)

// The request bodies of the routes whose permission depends on their body,
// and the route permission registry's resolvers for them
// (internal/edge/web route_rules.go, unified authn/z P1).
//
// Each body type is THE handler's request struct: the handler in api/manage
// or api/origins decodes into httpx.XxxBody directly (var req
// httpx.RepoCreateBody), so the handler and the resolver decode into one
// type. They live here, not beside their handlers, because an api package
// exports only Mount, only router.go and server.go may import one, and no
// family package may forward another's names through an alias
// (test/archtest web_split_test.go).
//
// Each resolver decodes the body with PeekJSON (DecodeJSON's decoder) into
// that type and applies the test the handler applies before its operator or
// admin gate, so the two cannot disagree about what a body says:
// case-insensitive field names, duplicate keys, an empty object. A body
// PeekJSON cannot decode resolves to the strongest permission.
//
// NOT YET ENFORCED, and not a replacement for the handlers' own checks: the
// in-handler gates (Admits(operator|admin) in api/manage and api/origins)
// must stay, rewritten at the flip (P4) as Policy.Has(repo:origin) and
// Policy.Has(secret:manage). They are the authority; these only choose the
// route-level permission, and some of those checks depend on stored state a
// resolver cannot see (a repo whose origin is linked to a stored secret).

// RepoCreateBodyMaxBytes is POST /repos's body limit, api/manage's
// maxCreateBodyBytes (6 x the 256 KiB ontology cap + 4 KiB, because
// ontology_yaml rides as an escaped JSON string). A manage test pins the two
// equal.
const RepoCreateBodyMaxBytes = 6*256*1024 + 4*1024

// RepoCreateBody is POST /repos (api/manage createRepoRequest).
type RepoCreateBody struct {
	Name           string `json:"name"`
	Mode           string `json:"mode"`
	OntologyPreset string `json:"ontology_preset"`
	OntologyYAML   string `json:"ontology_yaml"`
	// Template (F24): {repo, name}, repo by NAME and required. The third
	// ontology source, for modes "template" and "initialize".
	Template *struct {
		Repo string `json:"repo"`
		Name string `json:"name"`
	} `json:"template"`
	Origin *struct {
		URL        string `json:"url"`
		Branch     string `json:"branch"`
		AuthMethod string `json:"auth_method"`
		AuthToken  string `json:"auth_token"`
		// SecretID links the new repo's origin to a stored secret instead
		// of a pasted credential (admin only).
		SecretID string `json:"secret_id"`
	} `json:"origin"`
}

// ArchivedRestoreBody is POST /archived/{id}/restore (api/manage
// restoreRequest).
type ArchivedRestoreBody struct {
	NewName string `json:"new_name"`
	// The restored origin's credential, exactly one of: a stored secret, a
	// pasted credential, or none. Required when the archived repo has an
	// origin.
	SecretID     string `json:"secret_id"`
	AuthMethod   string `json:"auth_method"`
	Token        string `json:"token"`
	User         string `json:"user"`
	Password     string `json:"password"`
	NoCredential bool   `json:"no_credential"`
}

// ProbeOriginBody is POST /repos:probe-origin (api/manage probeOriginRequest).
type ProbeOriginBody struct {
	URL        string `json:"url"`
	Branch     string `json:"branch"`
	AuthMethod string `json:"auth_method"`
	AuthToken  string `json:"auth_token"`
	SecretID   string `json:"secret_id"`
}

// ProbeInitializedBody is POST /repos:probe-initialized (api/manage
// probeInitializedRequest).
type ProbeInitializedBody struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
	// Credentials are carried for the same reason probe-origin carries them:
	// an answer obtained anonymously is an answer about a different request.
	AuthMethod string `json:"auth_method"`
	AuthToken  string `json:"auth_token"`
	// SecretID probes with a stored secret instead (admin only).
	SecretID string `json:"secret_id"`
}

// OriginCredentialBody is PUT /repos/{repo}/origin/credential (api/manage
// setCredentialRequest). basic is user plus password (or a token already in
// user:password form). SecretID is raw: absent, a stored secret's id, or
// null (unlink).
type OriginCredentialBody struct {
	AuthMethod string          `json:"auth_method"`
	Token      string          `json:"token"`
	User       string          `json:"user"`
	Password   string          `json:"password"`
	SecretID   json.RawMessage `json:"secret_id"`
}

// FleetCreateBody is POST /fleet/create (api/manage createFleetRequest).
type FleetCreateBody struct {
	Name   string `json:"name,omitempty"`
	Origin *struct {
		URL        string `json:"url"`
		Branch     string `json:"branch,omitempty"`
		AuthMethod string `json:"auth_method,omitempty"`
		AuthToken  string `json:"auth_token,omitempty"`
		// SecretID hosts the fleet on the forge with a stored secret
		// instead of a pasted credential.
		SecretID string `json:"secret_id,omitempty"`
	} `json:"origin,omitempty"`
}

// FleetJoinBody is POST /fleet/join and /fleet/join:check (api/manage
// joinRequest).
type FleetJoinBody struct {
	AuthMethod string `json:"auth_method,omitempty"`
	AuthToken  string `json:"auth_token,omitempty"`
	// SecretID: a stored secret for a forge-hosted fleet, instead of a
	// pasted credential.
	SecretID string `json:"secret_id,omitempty"`
	Bundle   string `json:"bundle"`
}

// FleetFollowBody is POST /fleet/follow (api/manage followRequest).
type FleetFollowBody struct {
	URL        string `json:"url"`
	AuthMethod string `json:"auth_method,omitempty"`
	AuthToken  string `json:"auth_token,omitempty"`
	User       string `json:"user,omitempty"`
	Password   string `json:"password,omitempty"`
	// SecretID: a stored secret for the forge instead of a pasted
	// credential ("Update access" with a stored secret, or a move).
	SecretID string `json:"secret_id,omitempty"`
}

// OriginSessionBody is POST /repos/{repo}/origin-sessions/ (api/origins
// createSessionRequest).
type OriginSessionBody struct {
	URL        string `json:"url"`
	AuthMethod string `json:"auth_method"`
	Token      string `json:"token"`
	User       string `json:"user"`
	Password   string `json:"password"`
	// SecretID tests and commits with a stored secret instead of a pasted
	// credential (admin only; the secret must fit the URL).
	SecretID string `json:"secret_id"`
}

// ResolveRepoCreate is POST /repos: an origin (any origin object, even {})
// needs repo:origin (handleHALReposCreate's operator gate), an origin naming
// a stored secret secret:manage (admitSecretID's admin gate), anything else
// repo:manage.
func ResolveRepoCreate(r *http.Request) authz.Permission {
	var req RepoCreateBody
	if !PeekJSON(r, &req, RepoCreateBodyMaxBytes) {
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
	var req ArchivedRestoreBody
	if !PeekJSON(r, &req, 0) {
		return authz.SecretManage
	}
	switch {
	case req.SecretID != "":
		return authz.SecretManage
	case req.AuthMethod != "" || AssembleAuthToken(req.AuthMethod, req.Token, req.User, req.Password) != "":
		return authz.RepoOrigin
	}
	return authz.RepoManage
}

// ResolveProbeOrigin is POST /repos:probe-origin: secret_id needs
// secret:manage (admitSecretID), anything else repo:origin.
func ResolveProbeOrigin(r *http.Request) authz.Permission {
	var req ProbeOriginBody
	if !PeekJSON(r, &req, 0) || req.SecretID != "" {
		return authz.SecretManage
	}
	return authz.RepoOrigin
}

// ResolveProbeInitialized is POST /repos:probe-initialized, as
// ResolveProbeOrigin (its handler reads up to the create limit).
func ResolveProbeInitialized(r *http.Request) authz.Permission {
	var req ProbeInitializedBody
	if !PeekJSON(r, &req, RepoCreateBodyMaxBytes) || req.SecretID != "" {
		return authz.SecretManage
	}
	return authz.RepoOrigin
}

// ResolveOriginCredential is PUT /repos/{repo}/origin/credential: ANY
// secret_id — a stored secret's id, or null, which unlinks one — needs
// secret:manage (handleSetOriginCredential's admin gate after
// parseSecretIDField), anything else repo:origin.
func ResolveOriginCredential(r *http.Request) authz.Permission {
	var req OriginCredentialBody
	if !PeekJSON(r, &req, 0) || len(req.SecretID) > 0 {
		return authz.SecretManage
	}
	return authz.RepoOrigin
}

// ResolveFleetCreate is POST /fleet/create: an origin naming a stored secret
// needs secret:manage, anything else fleet:manage.
func ResolveFleetCreate(r *http.Request) authz.Permission {
	var req FleetCreateBody
	if !PeekJSON(r, &req, 0) || (req.Origin != nil && req.Origin.SecretID != "") {
		return authz.SecretManage
	}
	return authz.FleetManage
}

// ResolveFleetJoin is POST /fleet/join and /fleet/join:check (one body,
// decodeJoinRequest): secret_id needs secret:manage.
func ResolveFleetJoin(r *http.Request) authz.Permission {
	var req FleetJoinBody
	if !PeekJSON(r, &req, 0) || req.SecretID != "" {
		return authz.SecretManage
	}
	return authz.FleetManage
}

// ResolveFleetFollow is POST /fleet/follow: secret_id needs secret:manage.
func ResolveFleetFollow(r *http.Request) authz.Permission {
	var req FleetFollowBody
	if !PeekJSON(r, &req, 0) || req.SecretID != "" {
		return authz.SecretManage
	}
	return authz.FleetManage
}

// ResolveOriginSession is POST /repos/{repo}/origin-sessions/: secret_id
// needs secret:manage (the handler's admin gate), anything else repo:origin.
// The commit leg's admin check depends on whether the repo is ALREADY linked,
// which no resolver can see: it stays in the handler.
func ResolveOriginSession(r *http.Request) authz.Permission {
	var req OriginSessionBody
	if !PeekJSON(r, &req, 0) || req.SecretID != "" {
		return authz.SecretManage
	}
	return authz.RepoOrigin
}
