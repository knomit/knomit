package manage

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
)

// setCredentialRequest is the JSON body for PUT /repos/{repo}/origin/credential.
// basic is user plus password (or a token already in user:password form).
type setCredentialRequest struct {
	AuthMethod string          `json:"auth_method"`
	Token      string          `json:"token"`
	User       string          `json:"user"`
	Password   string          `json:"password"`
	SecretID   json.RawMessage `json:"secret_id"`
}

// handleSetOriginCredential serves PUT /repos/{repo}/origin/credential: it
// replaces the origin's pasted credential WITHOUT reconnecting (no probe, no
// guard, no Sync restart; URL, branch and mode unchanged) and resets the
// sync back-off so the new credential is tried at once.
//
// admin is the local-or-same-origin admin gate that linking and unlinking a
// stored secret sit behind; the route itself is the operator's.
func handleSetOriginCredential(b hal.URLBuilder, m *repos.Manager, admin func(http.Handler) http.Handler) http.HandlerFunc {
	_ = admin // unused while the endpoint takes pasted credentials only, which need operator
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		var req setCredentialRequest
		if !httpx.DecodeJSON(w, r, &req, 0) {
			return
		}
		if len(req.SecretID) > 0 {
			hal.WriteProblem(w, http.StatusBadRequest, "Bad Request",
				"secret_id is not accepted by this endpoint: send auth_method with token, or with user and password",
				r.URL.Path)
			return
		}
		c := repos.CredentialChange{
			AuthMethod: req.AuthMethod,
			AuthToken:  httpx.AssembleAuthToken(req.AuthMethod, req.Token, req.User, req.Password),
		}
		if err := m.ChangeOriginCredential(r.Context(), ri, c, false); err != nil {
			writeCredentialError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, map[string]any{
			"status": "ok",
			"_links": map[string]any{
				"self":   map[string]string{"href": b.Repo(repoName) + "/origin/credential"},
				"origin": map[string]string{"href": b.Repo(repoName) + "/origin"},
			},
		})
	}
}

func writeCredentialError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, repos.ErrLinkedRepo):
		hal.WriteProblem(w, http.StatusConflict, "Linked to a stored secret", err.Error(), r.URL.Path)
	case errors.Is(err, control.ErrOriginChanged):
		hal.WriteProblem(w, http.StatusConflict, "Origin changed",
			"the origin was changed while the credential was being replaced; nothing was changed, so read the origin and retry",
			r.URL.Path)
	case errors.Is(err, repos.ErrNoOrigin):
		hal.WriteProblem(w, http.StatusConflict, "No origin", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrInvalidCredential):
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid credential", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrPeerCredential):
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid origin", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrManagerStopped):
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Server stopping", err.Error(), r.URL.Path)
	case httpx.StoreUnavailable(err):
		httpx.WriteStoreUnavailable(w, r)
	default:
		hal.WriteProblem(w, http.StatusInternalServerError, "Failed to replace credential", err.Error(), r.URL.Path)
	}
}
