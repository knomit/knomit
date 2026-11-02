package manage

import (
	"net/http"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
)

// admitSecretID vets a request's "secret_id" (an alternative to a pasted
// credential on create, probe and the origin-session endpoints) for an
// origin at url. With no secret_id it admits and returns "". Otherwise it
// refuses a pasted credential alongside (400), applies the admin gate
// (linking a stored secret is admin-only), and checks the secret fits url
// (422 for a host off its list or a type that does not fit the scheme). It
// returns the auth method the secret implies. On a refusal it has written
// the answer and reports false.
func admitSecretID(w http.ResponseWriter, r *http.Request, m *repos.Manager, admin func(http.Handler) http.Handler,
	secretID, url string, pasted bool) (string, bool) {
	if secretID == "" {
		return "", true
	}
	if pasted {
		hal.WriteProblem(w, http.StatusBadRequest, "Bad Request",
			"send either secret_id or a pasted credential (auth_token), not both", r.URL.Path)
		return "", false
	}
	if !httpx.Admits(admin, w, r) {
		return "", false
	}
	method, err := m.CheckSecretForURL(secretID, url)
	if err != nil {
		writeSecretFitError(w, r, err)
		return "", false
	}
	return method, true
}
