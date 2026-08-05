package web

import (
	"context"
	"encoding/json"
	"errors"
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
type originProvider interface {
	GetOrigin(ctx context.Context, ri *repos.RepoInstance) (*store.Remote, error)
	SetOrigin(ctx context.Context, ri *repos.RepoInstance, req setOriginRequest) error
	SetOriginUpstream(ctx context.Context, ri *repos.RepoInstance, branch string) error
	DeleteOrigin(ctx context.Context, ri *repos.RepoInstance) error
}

// defaultOriginProvider is the production originProvider. Reads come from the
// store; WRITES go through repos.Manager.SetOrigin/ClearOrigin, which record
// the origin and its credential in control.db before touching the store — the
// order that survives losing a repo's database.
//
// It therefore holds a Manager (injected by storeProviders.withDefaults). The
// local-origin policy is still enforced upstream in handleHALSetOrigin, so the
// gate cannot be silently skipped by a provider constructed elsewhere.
type defaultOriginProvider struct{ m *repos.Manager }

func (defaultOriginProvider) GetOrigin(_ context.Context, ri *repos.RepoInstance) (*store.Remote, error) {
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

func (p defaultOriginProvider) SetOrigin(ctx context.Context, ri *repos.RepoInstance, req setOriginRequest) error {
	// The credential a partial update falls back to comes from control.db, not
	// from the store: the store's auth columns are empty by design now, so
	// reading them here would turn every "re-point the URL, keep my token"
	// request into a silent deauthentication.
	var storedMethod, storedToken string
	if reg := p.m.RepoRegistry(); reg != nil {
		var cerr error
		storedMethod, storedToken, cerr = reg.OriginCredential(ri.Name())
		if cerr != nil {
			return cerr
		}
	}

	var (
		spec                   repos.OriginSpec
		interval, pushInterval = 300, 300
		resolveErr             error
	)
	// WithRead's OWN error matters: it does not call fn at all when no store is
	// attached, so ignoring it would let an unavailable store fall through as a
	// successful no-op write.
	if err := ri.WithRead(func(svc *store.Service) {
		// Load existing remote to support partial updates.
		existing, _ := svc.Remote().GetRemote("origin")

		// Resolve URL: use request value, fall back to existing.
		u := req.URL
		if u == "" && existing != nil {
			u = existing.URL
		}
		if u == "" {
			resolveErr = errOriginURLRequired
			return
		}
		if req.URL != "" && !isGitURL(req.URL) {
			resolveErr = errOriginInvalidURL
			return
		}

		// Resolve auth.
		authMethod := req.AuthMethod
		if authMethod == "" {
			authMethod = storedMethod
		}
		authToken := assembleAuthToken(authMethod, req.Token, req.User, req.Password)
		if authToken == "" {
			authToken = storedToken
		}

		// Validate URL/auth compatibility.
		if verr := validateURLAuth(u, authMethod); verr != nil {
			resolveErr = verr
			return
		}

		// Preserve existing intervals or use defaults.
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

		spec = repos.OriginSpec{URL: u, Branch: upstreamMain, AuthMethod: authMethod, AuthToken: authToken}
	}); err != nil {
		return errOriginNoStore
	}
	if resolveErr != nil {
		return resolveErr
	}
	return p.m.SetOrigin(ctx, ri.Name(), spec, interval, pushInterval)
}

func (defaultOriginProvider) SetOriginUpstream(_ context.Context, ri *repos.RepoInstance, branch string) error {
	var err error
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errOriginNoStore
			return
		}
		err = svc.Remote().SetUpstreamBranch("origin", branch, ri.AgentBranch())
	})
	return err
}

func (p defaultOriginProvider) DeleteOrigin(ctx context.Context, ri *repos.RepoInstance) error {
	// ClearOrigin forgets the credential and the origin row in control.db
	// BEFORE unwiring the store. A failure to unwire is logged there rather
	// than returned: control.db no longer names the origin, so the leftover
	// remote row is stale wiring the next boot removes — reporting it as a
	// failed disconnect would invite a retry that has nothing left to do.
	if err := p.m.ClearOrigin(ctx, ri.Name()); err != nil {
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

		if err := op.SetOrigin(r.Context(), ri, req); err != nil {
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

		// No write-through call here: op.SetOrigin went through
		// repos.Manager.SetOrigin, which wrote control.db FIRST — before the
		// store, and so necessarily before the ActivateSync below that can fail
		// the request with a 502. The origin most likely to need re-cloning
		// later is exactly the one whose first sync failed, and it is recorded.

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
// m is taken for the registry write-through alone: the upstream branch is half
// of the origin control.db records, and a repo re-pinned to "master" that was
// rebuilt from a registry still saying "main" would fetch a refspec the remote
// does not have.
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

		if err := op.SetOriginUpstream(r.Context(), ri, req.Branch); err != nil {
			if err == errOriginNoStore {
				hal.WriteProblem(w, http.StatusInternalServerError, "No store available",
					err.Error(), r.URL.Path)
				return
			}
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to set upstream branch",
				err.Error(), r.URL.Path)
			return
		}
		m.RecordOrigin(repoName)

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
//
// A disconnect is an origin change like any other and must reach control.db: a
// registry that kept the URL the user just removed would have the next boot
// silently re-clone this repo from it if the database were ever lost — using a
// credential they may well have revoked in the same breath. The provider's
// ClearOrigin does that first, so this handler needs no write-through of its
// own.
func handleHALDeleteOrigin(b hal.URLBuilder, op originProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())

		if err := op.DeleteOrigin(r.Context(), ri); err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to delete origin",
				err.Error(), r.URL.Path)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
