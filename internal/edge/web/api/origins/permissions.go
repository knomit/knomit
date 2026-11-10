package origins

import (
	"net/http"

	"knomit/internal/edge/web/httpx"
	"knomit/internal/foundation/authz"
)

// ResolveCreateSession is the route permission registry's resolver for POST
// /repos/{repo}/origin-sessions/ (internal/edge/web route_rules.go, unified
// authn/z P1): it decodes the body into the handler's own
// createSessionRequest with httpx.PeekJSON (DecodeJSON's decoder), so it
// sees secret_id exactly as the handler does. secret_id needs secret:manage
// (the handler's admin gate), anything else repo:origin; an undecodable body
// the strongest.
//
// Not yet enforced, and not a replacement for the handler's own admin
// checks, which must stay (as Policy.Has(secret:manage) after the flip): the
// commit leg's check depends on whether the repo is ALREADY linked to a
// stored secret, which no resolver can see.
func ResolveCreateSession(r *http.Request) authz.Permission {
	var req createSessionRequest
	if !httpx.PeekJSON(r, &req, 0) || req.SecretID != "" {
		return authz.SecretManage
	}
	return authz.RepoOrigin
}
