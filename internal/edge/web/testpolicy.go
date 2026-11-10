package web

import (
	"net/http"

	"knomit/internal/foundation/authz"
)

// injectedPolicy is the in-process test seam for the unified authn/z model:
// when non-nil, the authenticator asks it for the request's policy instead of
// authenticating. It is nil in production and only _test.go files assign it
// (test/archtest TestInjectedPolicy_AssignedOnlyFromTestFiles pins that).
// The authenticator (authenticate.go) consults it first, in exactly the two
// shapes rule 1 of that archtest allows; the authenticator itself is not
// mounted until the flip (P4).
var injectedPolicy func(*http.Request) (authz.Policy, bool)
