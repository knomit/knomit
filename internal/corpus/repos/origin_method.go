package repos

import (
	"net/http"
	"strings"
)

// MethodMismatch is the one scheme/auth-method rule for a remote URL: an ssh
// method on an http(s) URL, or a token or basic credential on an ssh URL,
// does not fit. It is nil for anything else ("none" on an ssh URL is the
// deliberate force-anonymous override and is not refused). It classifies a
// remote URL for an auth method, not a fact reference, which is why this
// file alone is exempt from TestRefClassification_HasNoSecondImplementation.
// The REST edge (web httpx.ValidateURLAuth) and the fleet follow
// (refuseMethodForURL) both call it; the wizard's urlAuthMismatch
// (web/src/originAuth.ts) says the same in the same words.
func MethodMismatch(u, method string) error {
	isSSH := strings.HasPrefix(u, "git@") || strings.HasPrefix(u, "ssh://")
	isHTTP := strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
	switch {
	case isHTTP && method == "ssh":
		return errSSHOnHTTP
	case isSSH && (method == "token" || method == "basic"):
		return errTokenOnSSH
	}
	return nil
}

var (
	errSSHOnHTTP  = &methodMismatch{"SSH auth cannot be used with HTTP/HTTPS URLs — use a token or basic auth instead"}
	errTokenOnSSH = &methodMismatch{"token/basic auth cannot be used with SSH URLs — use SSH auth instead"}
)

type methodMismatch struct{ msg string }

func (e *methodMismatch) Error() string { return e.msg }

// refuseMethodForURL is MethodMismatch for a follow whose URL is the stored
// one (no url in the body, so the REST edge had nothing to check the method
// against, #529 N-d): 422 auth_invalid.
func refuseMethodForURL(u, method string) error {
	if err := MethodMismatch(u, method); err != nil {
		return fleetErr(http.StatusUnprocessableEntity, "auth_invalid", err.Error())
	}
	return nil
}
