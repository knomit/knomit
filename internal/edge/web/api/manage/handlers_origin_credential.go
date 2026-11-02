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
// changes the origin's credential WITHOUT reconnecting (no probe, no guard,
// no Sync restart; URL, branch and mode unchanged) and resets the sync
// back-off so the new credential is tried at once. The body is one of:
//
//   - auth_method with token, or with user and password: a pasted
//     credential (refused with 409 on a repo linked to a stored secret);
//   - "secret_id": "<id>": link the stored secret (it must fit the origin's
//     host and scheme, 422 otherwise);
//   - "secret_id": null: unlink, replacing the link with the pasted
//     credential given alongside, or with none when none is given.
//
// admin is the local-or-same-origin admin gate that linking and unlinking
// sit behind; the route itself is the operator's.
func handleSetOriginCredential(b hal.URLBuilder, m *repos.Manager, admin func(http.Handler) http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		var req setCredentialRequest
		if !httpx.DecodeJSON(w, r, &req, 0) {
			return
		}
		secretID, unlink, ok := parseSecretIDField(w, r, req.SecretID)
		if !ok {
			return
		}
		pasted := req.AuthMethod != "" || req.Token != "" || req.User != "" || req.Password != ""
		if secretID != "" && pasted {
			hal.WriteProblem(w, http.StatusBadRequest, "Bad Request",
				"send either secret_id or a pasted credential (auth_method with token, or with user and password), not both",
				r.URL.Path)
			return
		}
		if (secretID != "" || unlink) && !httpx.Admits(admin, w, r) {
			return
		}
		c := repos.CredentialChange{
			AuthMethod: req.AuthMethod,
			AuthToken:  httpx.AssembleAuthToken(req.AuthMethod, req.Token, req.User, req.Password),
			SecretID:   secretID,
		}
		if err := m.ChangeOriginCredential(r.Context(), ri, c, unlink); err != nil {
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

// parseSecretIDField reads a request's optional "secret_id": absent (""),
// a string (link that secret) or null (unlink). Anything else is 400.
func parseSecretIDField(w http.ResponseWriter, r *http.Request, raw json.RawMessage) (id string, unlink, ok bool) {
	if len(raw) == 0 {
		return "", false, true
	}
	if string(raw) == "null" {
		return "", true, true
	}
	if err := json.Unmarshal(raw, &id); err != nil || id == "" {
		hal.WriteProblem(w, http.StatusBadRequest, "Bad Request",
			"secret_id must be a stored secret's id (list them with GET /secrets) or null to unlink", r.URL.Path)
		return "", false, false
	}
	return id, false, true
}

func writeCredentialError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, repos.ErrLinkedRepo):
		hal.WriteProblem(w, http.StatusConflict, "Linked to a stored secret", err.Error(), r.URL.Path)
	case errors.Is(err, control.ErrOriginChanged):
		hal.WriteProblem(w, http.StatusConflict, "Origin changed",
			"the origin was changed concurrently: read it with GET /repos/{repo}/origin, then retry the credential change",
			r.URL.Path)
	case errors.Is(err, control.ErrSecretHostNotAllowed), errors.Is(err, repos.ErrSecretTypeMismatch),
		errors.Is(err, control.ErrSecretNotFound):
		writeSecretFitError(w, r, err)
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
