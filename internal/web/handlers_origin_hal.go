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

// originsOf returns the manager's control.db origin tenant, refusing rather
// than handing back nil.
//
// Manager.Close nils m.origins under m.mu, and Origins' own methods dereference
// o.crypt/o.db on their first line — so a bare m.Origins().Set(...) panics
// against a manager that has shut down. That window is real: cmd/serve.go
// returns srv.Shutdown(shutCtx) after a 5s deadline and only then runs the
// deferred Close, so any request still in the handler past that deadline
// outlives the tenant it is about to write to. middleware.Recoverer would turn
// the panic into a 500, but through the crash-bundle path rather than as an
// answer. persistSessionOrigin guards this same window, and
// repos.controlHandles/ErrManagerStopped exist for it on the lifecycle side.
func originsOf(m *repos.Manager) (*repos.Origins, error) {
	o := m.Origins()
	if o == nil {
		return nil, repos.ErrManagerStopped
	}
	return o, nil
}

// restoreRemoteConfig puts the git remote back the way prev describes it, after
// a ConfigureRemote succeeded but the control.db write that was supposed to
// make it durable did not.
//
// It exists because the git config and control.db are two stores and only one
// of them can be written first. The git write happens first (see SetOrigin),
// which means the window between the two is one where go-git would fetch and
// push through the NEW url while GetRemote — and therefore the reconcile loop's
// auth and refspec — still answer with the OLD injected origin. That is not a
// theoretical window: Origins.Set refuses any non-empty credential when the
// agent key could not be read (crypt == nil), so the second write fails
// deterministically on a whole class of installs while the first has already
// re-pointed the repo. Restoring here is what makes a failed PUT/DELETE mean
// "nothing changed" rather than "half of it changed, quietly".
//
// prev is the injected origin read BEFORE the write; nil (or an empty URL)
// means there was none, and the restoration is to have no git remote at all.
// That path goes through Remote().DeleteRemote, which also drops the remotes
// status row — status is derived state, rewritten by the next sync, and there
// is no status worth preserving for an origin that was never configured.
//
// A failed restore is logged rather than returned: the caller is already
// returning the real error, and replacing it with the rollback's would hide the
// reason the write failed. The log line is the only trace of a repo left
// pointing at a url nothing else records, so it is an Error.
func restoreRemoteConfig(svc *store.Service, ri *repos.RepoInstance, prev *store.Remote, op string) {
	var rerr error
	if prev != nil && prev.URL != "" {
		rerr = svc.ConfigureRemote(prev.URL, prev.Branch, ri.AgentBranch())
	} else {
		rerr = svc.Remote().DeleteRemote("origin")
	}
	if rerr != nil {
		log.Error().Err(rerr).Str("op", op).Str("repo", ri.Name()).
			Msg("origin: failed to restore git remote after a failed durable write; " +
				"the repo's git remote may not match its stored origin until restart")
	}
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

// SetOrigin rewires the running store's git remote and then persists the
// connection identity that produced it: svc.ConfigureRemote rewrites the
// fetch/push refspecs so the reconcile loop picks the new origin up without a
// restart, Origins.Set makes it durable in control.db, and svc.SetOrigin makes
// GetRemote reflect it immediately. The repo's own remotes row (status only) is
// untouched by this write — see remote.go's GetRemote for why identity no
// longer lives there.
//
// That order is SetOriginUpstream's, for SetOriginUpstream's reason: the git
// write is the fallible one, so nothing may record success ahead of it.
// Persisting first and failing in ConfigureRemote leaves control.db reporting a
// URL the refspecs were never rewritten for, which the next boot silently
// adopts — a PUT that answered 500 taking effect on restart.
//
// Failing the other way round is NOT self-healing, which is why the rollback
// below is part of the ordering and not decoration. Between ConfigureRemote and
// Origins.Set the git remote names the new url while the injected origin — and
// so GetRemote, and so the reconcile loop's auth and upstream branch — still
// answer with the old one: the loop fetches the NEW url carrying the OLD
// credential, on every tick, until a restart re-derives the git config from the
// record that was never written. restoreRemoteConfig closes that window by
// putting the git remote back before the error is returned, so a failed PUT
// leaves the repo exactly as it found it.
func (defaultOriginProvider) SetOrigin(_ context.Context, m *repos.Manager, ri *repos.RepoInstance, req setOriginRequest) error {
	origins, oerr := originsOf(m)
	if oerr != nil {
		return oerr
	}
	var err error
	if aerr := ri.WithRead(func(svc *store.Service) {
		// Load existing remote to support partial updates — and to have
		// something to restore the git remote from if the durable write below
		// fails. The read error is no longer discarded precisely because of
		// that second job: "no origin" is (nil, nil) here, so a non-nil error
		// means the status row could not be read at all, and treating that as
		// "there was no origin" would make the rollback path delete the git
		// remote of a repo that has one.
		existing, gerr := svc.Remote().GetRemote("origin")
		if gerr != nil {
			err = gerr
			return
		}

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

		if cerr := svc.ConfigureRemote(u, upstreamMain, ri.AgentBranch()); cerr != nil {
			err = cerr
			return
		}
		if serr := origins.Set(ri.UID(), repos.Origin{
			URL:        u,
			Branch:     upstreamMain,
			AuthMethod: authMethod,
			AuthToken:  authToken,
		}); serr != nil {
			err = serr
			// The git remote now names a url control.db does not record. Put it
			// back before returning — see restoreRemoteConfig.
			restoreRemoteConfig(svc, ri, existing, "SetOrigin")
			return
		}
		svc.SetOrigin(&store.Origin{
			URL:        u,
			Branch:     upstreamMain,
			AuthMethod: authMethod,
			AuthToken:  authToken,
		})
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
//
// The window the order opens instead is SetOrigin's, in miniature: a refspec
// that fetches the new branch while GetRemote still reports the old one leaves
// reconcileNow reconciling against a refs/remotes/origin/<old> nothing updates
// any more. So the same rollback applies — restore the refspec, then return the
// error.
func (defaultOriginProvider) SetOriginUpstream(_ context.Context, m *repos.Manager, ri *repos.RepoInstance, branch string) error {
	origins, oerr := originsOf(m)
	if oerr != nil {
		return oerr
	}
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
		if serr := origins.SetBranch(ri.UID(), branch); serr != nil {
			err = serr
			restoreRemoteConfig(svc, ri, existing, "SetOriginUpstream")
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

// DeleteOrigin drops the git remote, deletes the durable record from
// control.db, and only THEN clears the injected origin on the running store.
//
// Every fallible step runs before the one that cannot fail, and the injected
// origin is that one. It goes last because clearing it is what SILENCES the
// repo: runReconcileLoop's tick reads GetRemote and returns on a nil origin
// without logging (there is nothing to report about a repo that has no remote),
// so nil-ing it ahead of a step that then fails would answer 500 while leaving
// the repo permanently, invisibly unsynced — git remote still configured,
// credential still stored, and no trace in the log until someone notices the
// facts stopped arriving.
//
// The control.db row is deleted before, not after, that point for the reason
// the previous ordering was wrong the other way: the old code deleted the row
// FIRST, before anything that could fail, and discarded ri.WithRead's return —
// which reports Acquire's error WITHOUT running the closure. Against a detached
// store (a swap in flight, a failed recovery reopen) err stayed nil, the
// handler answered 204, and the URL, auth_method and encrypted auth_token were
// gone while the git remote was still configured and still pushing. Since this
// branch moved connection identity out of the repo database, that token existed
// nowhere else. The order here keeps both properties: the record outlives every
// step that can fail, and nothing goes quiet until all of them have succeeded.
func (defaultOriginProvider) DeleteOrigin(_ context.Context, m *repos.Manager, ri *repos.RepoInstance) error {
	origins, oerr := originsOf(m)
	if oerr != nil {
		return oerr
	}
	var err error
	if aerr := ri.WithRead(func(svc *store.Service) {
		// Read before tearing anything down: this is what a failed teardown is
		// restored from. (nil, nil) means there is no origin, which makes the
		// whole body idempotent — both deletes below tolerate absence.
		existing, gerr := svc.Remote().GetRemote("origin")
		if gerr != nil {
			err = gerr
			return
		}
		if derr := svc.Remote().DeleteRemote("origin"); derr != nil {
			err = derr
			return
		}
		if derr := origins.Delete(ri.UID()); derr != nil {
			err = derr
			// The git remote is gone but the record is not. Put the remote back
			// so the repo keeps syncing through the origin the caller failed to
			// remove, rather than sitting on a record it can no longer act on.
			// Sync/push status is not restored with it — that is derived state
			// the next tick rewrites.
			restoreRemoteConfig(svc, ri, existing, "DeleteOrigin")
			return
		}
		svc.SetOrigin(nil)
	}); aerr != nil {
		return acquireFailed(aerr)
	}
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

		// A remote that is a DIFFERENT knowledge base cannot govern this repo.
		// Create enforces this on both of its remote modes; attaching an origin
		// is the other door into the same situation, and the repo's ontology is
		// fixed at create time, so there is no way back from getting it wrong.
		//
		// Before SetOrigin, not after: the origin row and the git remote are
		// written there, and a refusal that arrives afterwards has already
		// pointed the repo at the remote it is refusing.
		if req.URL != "" && ri.Ontology() != nil {
			if err := m.CheckOriginOntology(r.Context(), ri.Ontology().ID, repos.OriginSpec{
				URL:        req.URL,
				Branch:     req.Branch,
				AuthMethod: req.AuthMethod,
				AuthToken:  assembleAuthToken(req.AuthMethod, req.Token, req.User, req.Password),
			}); err != nil {
				hal.WriteProblem(w, http.StatusConflict, "Different knowledge base",
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
