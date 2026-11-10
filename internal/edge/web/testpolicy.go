package web

import (
	"net/http"

	"knomit/internal/foundation/authz"
)

// injectedPolicy is the in-process test seam for the unified authn/z model:
// when non-nil, the authenticator asks it for the request's policy instead of
// authenticating. It is nil in production and only _test.go files assign it
// (test/archtest TestInjectedPolicy_AssignedOnlyFromTestFiles pins that).
// Nothing reads it yet; the gate that does arrives with the flip.
var injectedPolicy func(*http.Request) (authz.Policy, bool)
