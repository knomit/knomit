package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

var (
	errOriginNoStore     = errors.New("no store available")
	errOriginURLRequired = errors.New("url is required")
	errOriginInvalidURL  = errors.New("invalid url")
)

// originProvider is the narrow interface the origin HAL handlers depend on.
// Tests inject a stub; production wires through RepoInstance.WithRead.
//
// The ctx these methods take is currently unused by the default provider: the
// store's Remote sub-service (svc.Remote()) takes no context, so there is
// nothing below to hand it to. It is threaded anyway so that giving Remote a
// ctx — a small store-layer follow-up — needs no second signature change here,
// and so this interface matches every sibling provider rather than being the
// one exception a reader has to explain to themselves.
//
// SetOrigin/SetOriginUpstream/DeleteOrigin take the Manager because they write
// through it: connection identity (url/branch/auth) is control.db's now (via
// Manager.Origins()), not the repo's own store — a lost .db can be re-cloned
// from the record that outlives it. GetOrigin needs no Manager: the injected
// origin already makes GetRemote return the control.db-backed record.
type originProvider interface {
	GetOrigin(ctx context.Context, ri *repos.RepoInstance) (*store.Remote, error)
	SetOrigin(ctx context.Context, m *repos.Manager, ri *repos.RepoInstance, req setOriginRequest) error
	SetOriginUpstream(ctx context.Context, m *repos.Manager, ri *repos.RepoInstance, branch string) error
	DeleteOrigin(ctx context.Context, m *repos.Manager, ri *repos.RepoInstance) error
}

// defaultOriginProvider is the production originProvider backed by the store.
// It is pure storage; the local-origin policy is enforced upstream in
// handleHALSetOrigin (which holds the real Manager) so the gate can never be
// silently skipped by a provider constructed without it.
type defaultOriginProvider struct{}

// acquireFailed wraps a WithRead failure as errOriginNoStore.
//
// ri.WithRead returns Acquire's error WITHOUT invoking the closure — that is
// its documented contract — so every caller in this file must ASSIGN that
// return. Discarding it makes a detached store (a swap in flight, a failed
// recovery reopen) look like a successful no-op, which is how DeleteOrigin
// came to answer 204 having done nothing but destroy the record.
//
// Acquire never yields a nil service with a nil error, so the `svc == nil`
// branches these methods used to carry were unreachable; this is the real
// no-store signal, and it is now the only one.
func acquireFailed(err error) error {
	return fmt.Errorf("%w: %v", errOriginNoStore, err)
}

func (defaultOriginProvider) GetOrigin(_ context.Context, ri *repos.RepoInstance) (*store.Remote, error) {
	var (
		remote *store.Remote
		err    error
	)
	if aerr := ri.WithRead(func(svc *store.Service) {
		remote, err = svc.Remote().GetRemote("origin")
	}); aerr != nil {
		return nil, acquireFailed(aerr)
	}
	return remote, err
}

// SetOrigin persists connection identity to control.db (mgr.Origins()) and
// then updates the running store: svc.SetOrigin makes GetRemote reflect it
// immediately, and svc.ConfigureRemote rewires the git fetch/push refspecs so
// the reconcile loop uses it without a restart. The repo's own remotes row
// (status only) is untouched by this write — see remote.go's GetRemote for
// why identity no longer lives there.
func (defaultOriginProvider) SetOrigin(_ context.Context, m *repos.Manager, ri *repos.RepoInstance, req setOriginRequest) error {
	var err error
	if aerr := ri.WithRead(func(svc *store.Service) {
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

		if serr := m.Origins().Set(ri.UID(), repos.Origin{
			URL:        u,
			Branch:     upstreamMain,
			AuthMethod: authMethod,
			AuthToken:  authToken,
		}); serr != nil {
			err = serr
			return
		}
		svc.SetOrigin(&store.Origin{
			URL:        u,
			Branch:     upstreamMain,
			AuthMethod: authMethod,
			AuthToken:  authToken,
		})
		err = svc.ConfigureRemote(u, upstreamMain, ri.AgentBranch())
	}); aerr != nil {
		return acquireFailed(aerr)
	}
	return err
}

// SetOriginUpstream changes only the upstream branch, preserving the ordering
// discipline SetUpstreamBranch used to enforce in one place: rewrite the git
// fetch refspec FIRST (ConfigureRemote), and only on success touch the stored
// branch (Origins.SetBranch + svc.SetOrigin). A failure between the two used
// to be impossible because it was one function; splitting storage across
// control.db and the live store makes it possible again, so the order here is
// load-bearing — reordering would let a refspec rewrite fail while the stored
// branch (and the next GetRemote) already reports the new one.
func (defaultOriginProvider) SetOriginUpstream(_ context.Context, m *repos.Manager, ri *repos.RepoInstance, branch string) error {
	var err error
	if aerr := ri.WithRead(func(svc *store.Service) {
		existing, gerr := svc.Remote().GetRemote("origin")
		if gerr != nil {
			err = gerr
			return
		}
		if existing == nil || existing.URL == "" {
			err = fmt.Errorf("SetOriginUpstream: no origin configured")
			return
		}
		if cerr := svc.ConfigureRemote(existing.URL, branch, ri.AgentBranch()); cerr != nil {
			err = cerr
			return
		}
		if serr := m.Origins().SetBranch(ri.UID(), branch); serr != nil {
			err = serr
			return
		}
		svc.SetOrigin(&store.Origin{
			URL:        existing.URL,
			Branch:     branch,
			AuthMethod: existing.AuthMethod,
			AuthToken:  existing.AuthToken,
		})
	}); aerr != nil {
		return acquireFailed(aerr)
	}
	return err
}

// DeleteOrigin clears the injected origin on the running store (so GetRemote
// immediately reports none), drops the git remote, and only THEN deletes the
// durable record from control.db.
//
// That order is the fix, not a preference. The old code deleted the control.db
// row first, before anything that could fail, and then discarded ri.WithRead's
// return — which reports Acquire's error WITHOUT running the closure. Against a
// detached store (a swap in flight, a failed recovery reopen) err stayed nil,
// the handler answered 204, and the URL, auth_method and encrypted auth_token
// were gone while the git remote was still configured and still pushing. Since
// this branch moved connection identity out of the repo database, that token
// existed nowhere else: there was nothing left to recover from.
func (defaultOriginProvider) DeleteOrigin(_ context.Context, m *repos.Manager, ri *repos.RepoInstance) error {
	var err error
	if aerr := ri.WithRead(func(svc *store.Service) {
		svc.SetOrigin(nil)
		err = svc.Remote().DeleteRemote("origin")
	}); aerr != nil {
		return acquireFailed(aerr)
	}
	if err != nil {
		return err
	}
	if derr := m.Origins().Delete(ri.UID()); derr != nil {
		return derr
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
func handleHALGetOrigin(b hal.URLBuilder, op originProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		remote, err := op.GetOrigin(r.Context(), ri)
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
		ri := repos.RepoFromContext(r.Context())

		var req setOriginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body",
				err.Error(), r.URL.Path)
			return
		}

		// Enforce the local-origin policy here, at the write edge, where the real
		// Manager is in hand. PUT /origin defers the clone to the sync loop, so
		// this is the gate for that deferred path. A partial update (empty url)
		// reuses the stored URL, which was already gated when first written.
		if req.URL != "" {
			if err := m.ValidateLocalOrigin(req.URL); err != nil {
				hal.WriteProblem(w, http.StatusBadRequest, "Origin not allowed",
					err.Error(), r.URL.Path)
				return
			}
		}

		if err := op.SetOrigin(r.Context(), m, ri, req); err != nil {
			// errors.Is, not ==: the provider WRAPS the acquire failure that
			// carries the reason (ErrStoreUnavailable / ErrRepoClosed).
			switch {
			case errors.Is(err, errOriginNoStore):
				hal.WriteProblem(w, http.StatusInternalServerError, "No store available",
					err.Error(), r.URL.Path)
			case errors.Is(err, errOriginURLRequired):
				hal.WriteProblem(w, http.StatusBadRequest, "URL required",
					err.Error(), r.URL.Path)
			case errors.Is(err, errOriginInvalidURL):
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

// upstreamRequest is the JSON body for PATCH /repos/{repo}/origin/upstream.
type upstreamRequest struct {
	Branch string `json:"branch"`
}

// isValidUpstreamBranch applies a conservative subset of git's ref-name rules,
// enough to keep a caller-supplied branch from breaking the fetch refspec it is
// woven into (`+refs/heads/<branch>:refs/remotes/origin/<branch>`). It rejects
// control characters, spaces, the special ref characters git forbids, leading
// '-'/'/' and trailing '/', and the ".." / "@{" sequences.
func isValidUpstreamBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") {
		return false
	}
	if strings.Contains(b, "..") || strings.Contains(b, "@{") {
		return false
	}
	for _, r := range b {
		if r <= ' ' || r == 0x7f { // control characters and space
			return false
		}
		switch r {
		case '~', '^', ':', '?', '*', '[', '\\':
			return false
		}
	}
	return true
}

// handleHALSetOriginUpstream serves PATCH /repos/{repo}/origin/upstream.
//
// It changes ONLY the configured consensus ("main") branch of an existing
// origin, without re-running the connect/activate flow or touching auth. The
// running reconcile loop reads the remote record fresh each tick, so the new
// upstream takes effect on the next cycle. Use this to recover from a config
// where the upstream was mistakenly the agent branch (which forces push-only).
func handleHALSetOriginUpstream(b hal.URLBuilder, m *repos.Manager, op originProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		var req upstreamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body",
				err.Error(), r.URL.Path)
			return
		}
		if req.Branch == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Branch required",
				"branch is required", r.URL.Path)
			return
		}
		if !isValidUpstreamBranch(req.Branch) {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid branch name",
				"branch name contains characters not allowed in a git ref", r.URL.Path)
			return
		}

		if err := op.SetOriginUpstream(r.Context(), m, ri, req.Branch); err != nil {
			if errors.Is(err, errOriginNoStore) {
				hal.WriteProblem(w, http.StatusInternalServerError, "No store available",
					err.Error(), r.URL.Path)
				return
			}
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to set upstream branch",
				err.Error(), r.URL.Path)
			return
		}

		view := map[string]any{
			"status": "ok",
			"branch": req.Branch,
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
		ri := repos.RepoFromContext(r.Context())

		if err := op.DeleteOrigin(r.Context(), m, ri); err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to delete origin",
				err.Error(), r.URL.Path)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
