package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

var (
	errOriginNoStore    = errors.New("no store available")
	errOriginURLRequired = errors.New("url is required")
	errOriginInvalidURL  = errors.New("invalid url")
)

// originProvider is the narrow interface the origin HAL handlers depend on.
// Tests inject a stub; production wires through RepoInstance.WithRead.
type originProvider interface {
	GetOrigin(ri *repos.RepoInstance) (*store.Remote, error)
	SetOrigin(ri *repos.RepoInstance, req setOriginRequest) error
	DeleteOrigin(ri *repos.RepoInstance) error
}

// defaultOriginProvider is the production originProvider backed by the store.
type defaultOriginProvider struct{}

func (defaultOriginProvider) GetOrigin(ri *repos.RepoInstance) (*store.Remote, error) {
	var (
		remote *store.Remote
		err    error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		remote, err = svc.Remote().GetRemote("origin")
	})
	return remote, err
}

func (defaultOriginProvider) SetOrigin(ri *repos.RepoInstance, req setOriginRequest) error {
	var err error
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errOriginNoStore
			return
		}

		// Load existing remote to support partial updates.
		existing, _ := svc.Remote().GetRemote("origin")

		// Resolve URL: use request value, fall back to existing.
		u := req.URL
		if u == "" && existing != nil {
			u = existing.URL
		}
		if u == "" {
			err = errOriginURLRequired
			return
		}
		if req.URL != "" && !isGitURL(req.URL) {
			err = errOriginInvalidURL
			return
		}

		// Resolve auth.
		authMethod := req.AuthMethod
		if authMethod == "" && existing != nil {
			authMethod = existing.AuthMethod
		}
		authToken := assembleAuthToken(authMethod, req.Token, req.User, req.Password)
		if authToken == "" && existing != nil {
			authToken = existing.AuthToken
		}

		// Validate URL/auth compatibility.
		if verr := validateURLAuth(u, authMethod); verr != nil {
			err = verr
			return
		}

		// Preserve existing intervals or use defaults.
		branch := "main"
		interval := 300
		pushInterval := 300
		if existing != nil {
			branch = existing.Branch
			interval = existing.Interval
			pushInterval = existing.PushInterval
		}

		err = svc.Remote().SetRemote("origin", u, branch, interval, pushInterval, authMethod, authToken)
	})
	return err
}

func (defaultOriginProvider) DeleteOrigin(ri *repos.RepoInstance) error {
	// The legacy API has no delete; we model it as a no-op (204) for now.
	// A full implementation would call svc.Remote().DeleteRemote("origin").
	return nil
}

// originView is the HAL response body for GET /repos/{repo}/origin.
type originView struct {
	Name       string      `json:"name"`
	URL        string      `json:"url"`
	Branch     string      `json:"branch"`
	AuthMethod string      `json:"auth_method,omitempty"`
	Links      hal.LinkMap `json:"_links"`
}

func originSelfURL(b hal.URLBuilder, repo string) string {
	return b.Repo(repo) + "/origin"
}

// handleHALGetOrigin serves GET /repos/{repo}/origin.
// Returns 200 with HAL origin view, or 204 if no origin is configured.
func handleHALGetOrigin(b hal.URLBuilder, m *repos.Manager, op originProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		remote, err := op.GetOrigin(ri)
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to get origin",
				err.Error(), r.URL.Path)
			return
		}
		if remote == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		view := originView{
			Name:       remote.Name,
			URL:        remote.URL,
			Branch:     remote.Branch,
			AuthMethod: remote.AuthMethod,
			Links: hal.LinkMap{
				"self": {Href: originSelfURL(b, repoName)},
				"repo": {Href: b.Repo(repoName)},
			},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleHALSetOrigin serves PUT /repos/{repo}/origin.
// Accepts JSON body matching setOriginRequest. Returns 200 with HAL response.
func handleHALSetOrigin(b hal.URLBuilder, m *repos.Manager, op originProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		var req setOriginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body",
				err.Error(), r.URL.Path)
			return
		}

		if err := op.SetOrigin(ri, req); err != nil {
			switch err {
			case errOriginNoStore:
				hal.WriteProblem(w, http.StatusInternalServerError, "No store available",
					err.Error(), r.URL.Path)
			case errOriginURLRequired:
				hal.WriteProblem(w, http.StatusBadRequest, "URL required",
					err.Error(), r.URL.Path)
			case errOriginInvalidURL:
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid URL",
					err.Error(), r.URL.Path)
			default:
				hal.WriteProblem(w, http.StatusInternalServerError, "Failed to set origin",
					err.Error(), r.URL.Path)
			}
			return
		}

		// Attempt to activate sync; log failure but don't fail the request.
		if aerr := ri.ActivateSync(req.URL); aerr != nil {
			log.Warn().Err(aerr).Str("repo", repoName).Msg("sync activation failed")
		}

		view := map[string]any{
			"status": "ok",
			"_links": hal.LinkMap{
				"self": {Href: originSelfURL(b, repoName)},
				"repo": {Href: b.Repo(repoName)},
			},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleHALDeleteOrigin serves DELETE /repos/{repo}/origin.
// Returns 204 No Content on success.
func handleHALDeleteOrigin(b hal.URLBuilder, m *repos.Manager, op originProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		if err := op.DeleteOrigin(ri); err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to delete origin",
				err.Error(), r.URL.Path)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
