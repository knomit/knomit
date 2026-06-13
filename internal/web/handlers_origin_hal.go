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
		interval := 300
		pushInterval := 300
		if existing != nil {
			interval = existing.Interval
			pushInterval = existing.PushInterval
		}

		// Resolve the upstream consensus branch: explicit request > existing
		// remote record > "main". The HAL request lets master-default repos
		// pin the branch without going through the session-based flow.
		upstreamMain := req.Branch
		if upstreamMain == "" && existing != nil {
			upstreamMain = existing.Branch
		}
		if upstreamMain == "" {
			upstreamMain = "main"
		}

		err = svc.Remote().SetRemote("origin", u, upstreamMain, ri.AgentBranch(), interval, pushInterval, authMethod, authToken)
	})
	return err
}

func (defaultOriginProvider) DeleteOrigin(ri *repos.RepoInstance) error {
	var err error
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errOriginNoStore
			return
		}
		err = svc.Remote().DeleteRemote("origin")
	})
	if err != nil {
		return err
	}
	// Stop the sync loop now that the remote is gone.
	ri.DeactivateSync()
	return nil
}

// originView is the HAL response body for GET /repos/{repo}/origin. It mirrors
// the persisted remote record including sync/push status so the UI can show
// real last-sync state instead of guessing.
type originView struct {
	Name           string      `json:"name"`
	URL            string      `json:"url"`
	Branch         string      `json:"branch"`
	Interval       int         `json:"interval"`
	LastSyncAt     *string     `json:"last_sync_at"`
	LastStatus     *string     `json:"last_status"`
	LastError      *string     `json:"last_error"`
	PushInterval   int         `json:"push_interval"`
	LastPushAt     *string     `json:"last_push_at"`
	LastPushStatus *string     `json:"last_push_status"`
	LastPushError  *string     `json:"last_push_error"`
	AuthMethod     string      `json:"auth_method,omitempty"`
	Links          hal.LinkMap `json:"_links"`
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
			Name:           remote.Name,
			URL:            remote.URL,
			Branch:         remote.Branch,
			Interval:       remote.Interval,
			LastSyncAt:     remote.LastSyncAt,
			LastStatus:     remote.LastStatus,
			LastError:      remote.LastError,
			PushInterval:   remote.PushInterval,
			LastPushAt:     remote.LastPushAt,
			LastPushStatus: remote.LastPushStatus,
			LastPushError:  remote.LastPushError,
			AuthMethod:     remote.AuthMethod,
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

		// Activate sync now (synchronous initial reconcile). If it fails,
		// the origin row IS persisted — surfacing a 502 lets the operator
		// distinguish a bad token / unreachable origin from a successful
		// configure without forcing them to re-enter the URL. The session
		// flow (handlers_origin_session.go) returns the analogous error.
		if aerr := ri.ActivateSync(req.URL); aerr != nil {
			log.Warn().Err(aerr).Str("repo", repoName).Msg("sync activation failed")
			hal.WriteProblem(w, http.StatusBadGateway, "Sync activation failed",
				"origin was saved but the initial reconcile failed: "+aerr.Error(), r.URL.Path)
			return
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
