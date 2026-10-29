package manage

import (
	"context"
	"errors"
	"fmt"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos"

	"knomit/internal/corpus/repos/control"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/remotesync"
	"knomit/internal/foundation/pki"
)

var (
	errOriginURLRequired = errors.New("url is required")
	errOriginInvalidURL  = errors.New("invalid url")
	// errNoOrigin: PATCH /origin/upstream on a repo with no origin. There is
	// no branch to re-point and nothing to vet one against.
	errNoOrigin = errors.New("no origin configured")
)

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
func originsOf(m *repos.Manager) (*control.Origins, error) {
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
func restoreRemoteConfig(svc *store.Service, ri *repos.RepoInstance, prev *remotesync.Remote, op string) {
	var rerr error
	if prev != nil && prev.URL != "" {
		rerr = repos.ConfigureGitRemote(svc, prev.URL, prev.AuthMethod, prev.Branch, ri.AgentBranch())
	} else {
		rerr = svc.Remote().DeleteRemote("origin")
	}
	if rerr != nil {
		log.Error().Err(rerr).Str("op", op).Str("repo", ri.Name()).
			Msg("origin: failed to restore git remote after a failed durable write; " +
				"the repo's git remote may not match its stored origin until restart")
	}
}

// getOrigin reads a repo's origin for the origin HAL handlers (and for the
// GET /repos health column); it and setOriginUpstream are their store read and
// write, through RepoInstance.WithRead. Attaching and detaching an origin are
// NOT here: they are lifecycle events (AttachOrigin, DetachOrigin) sent to the
// repo's machine, which restarts the Sync stage around the change. Both are
// pure storage; the local-origin policy is enforced in handleHALSetOrigin,
// which holds the real Manager.
//
// The ctx they take is currently unused: the store's Remote sub-service
// (svc.Remote()) takes no context, so there is nothing below to hand it to.
// It is threaded anyway so that giving Remote a ctx (a small store-layer
// follow-up) needs no second signature change here.
//
// Both return WithRead's error AS-IS when the store cannot be reached
// (httpx.StoreUnavailable), and every handler answers it with httpx.WriteStoreUnavailable:
// 503 "Repo not ready", the one answer the web layer gives for that state.
// ri.WithRead returns Acquire's error WITHOUT invoking the closure — that is
// its documented contract — so every caller in this file must ASSIGN that
// return. Discarding it makes a detached store (a swap in flight, a failed
// recovery reopen) look like a successful no-op, which is how DeleteOrigin
// came to answer 204 having done nothing but destroy the record.
//
// setOriginUpstream takes the Manager because it writes through it: connection
// identity (url/branch/auth) is control.db's now (via Manager.Origins()), not
// the repo's own store, so a lost .db can be re-cloned from the record that
// outlives it. getOrigin needs no Manager: the injected origin already makes
// GetRemote return the control.db-backed record.
func getOrigin(_ context.Context, ri *repos.RepoInstance) (*remotesync.Remote, error) {
	var (
		remote *remotesync.Remote
		err    error
	)
	if aerr := ri.WithRead(func(svc *store.Service) {
		remote, err = svc.Remote().GetRemote("origin")
	}); aerr != nil {
		return nil, aerr
	}
	return remote, err
}

// setOriginUpstream changes only the upstream branch, preserving the ordering
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
//
// vettedURL is the origin URL the caller's CheckOriginBranch judged the new
// branch against. The change applies only while the origin is still that URL:
// Origins.SetBranch is a compare-and-set on it, in the UPDATE that makes the
// change durable, so an attach or detach that landed after the check is
// refused (control.ErrOriginChanged) instead of having the vetted branch
// applied to a remote nobody checked. On that refusal control.db records the
// write that won, so the git remote is restored to IT, not to what this
// closure read: restoring the stale read would re-point the git remote away
// from the origin that attach just stored.
func setOriginUpstream(_ context.Context, m *repos.Manager, ri *repos.RepoInstance, vettedURL, branch string) error {
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
			err = fmt.Errorf("SetOriginUpstream: %w", errNoOrigin)
			return
		}
		if cerr := repos.ConfigureGitRemote(svc, existing.URL, existing.AuthMethod, branch, ri.AgentBranch()); cerr != nil {
			err = cerr
			return
		}
		if serr := origins.SetBranch(ri.UID(), vettedURL, branch); serr != nil {
			err = serr
			prev := existing
			if errors.Is(serr, control.ErrOriginChanged) {
				prev = recordedOrigin(origins, ri, existing)
			}
			restoreRemoteConfig(svc, ri, prev, "SetOriginUpstream")
			return
		}
		svc.Remote().SetOrigin(&remotesync.Origin{
			URL:        existing.URL,
			Branch:     branch,
			AuthMethod: existing.AuthMethod,
			AuthToken:  existing.AuthToken,
		})
	}); aerr != nil {
		return aerr
	}
	return err
}

// recordedOrigin is the origin control.db records for ri now, as the git
// remote restore wants it: nil when there is none (a detach won). When it
// cannot be read, fallback is kept and the failure logged.
func recordedOrigin(origins *control.Origins, ri *repos.RepoInstance, fallback *remotesync.Remote) *remotesync.Remote {
	cur, err := origins.Get(ri.UID())
	if err != nil {
		log.Error().Err(err).Str("repo", ri.Name()).
			Msg("origin: cannot read the recorded origin after a refused upstream change; restoring the git remote to the one read before it")
		return fallback
	}
	if cur == nil {
		return nil
	}
	return &remotesync.Remote{URL: cur.URL, Branch: cur.Branch, AuthMethod: cur.AuthMethod}
}

// originView is the HAL response body for GET /repos/{repo}/origin. It mirrors
// the persisted remote record including sync/push status so the UI can show
// real last-sync state instead of guessing.
type originView struct {
	Name           string  `json:"name"`
	URL            string  `json:"url"`
	Branch         string  `json:"branch"`
	Interval       int     `json:"interval"`
	LastSyncAt     *string `json:"last_sync_at"`
	LastStatus     *string `json:"last_status"`
	LastError      *string `json:"last_error"`
	PushInterval   int     `json:"push_interval"`
	LastPushAt     *string `json:"last_push_at"`
	LastPushStatus *string `json:"last_push_status"`
	LastPushError  *string `json:"last_push_error"`
	AuthMethod     string  `json:"auth_method,omitempty"`
	// FetchBreaker and PushBreaker are the origin loop's circuit breakers
	// (F21 S1), in memory and read-only: closed before the loop's first
	// round. While one is open its step is skipped and writes no status, so
	// the last_* fields above keep the last REAL attempt.
	FetchBreaker repos.BreakerView `json:"fetch_breaker"`
	PushBreaker  repos.BreakerView `json:"push_breaker"`
	Links        hal.LinkMap       `json:"_links"`
}

func originSelfURL(b hal.URLBuilder, repo string) string {
	return b.Repo(repo) + "/origin"
}

// handleHALGetOrigin serves GET /repos/{repo}/origin.
// Returns 200 with HAL origin view, or 204 if no origin is configured.
func handleHALGetOrigin(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		remote, err := getOrigin(r.Context(), ri)
		if httpx.StoreUnavailable(err) {
			httpx.WriteStoreUnavailable(w, r)
			return
		}
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to get origin",
				err.Error(), r.URL.Path)
			return
		}
		if remote == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		fetchBrk, pushBrk := ri.SyncBreakers()
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
			FetchBreaker:   fetchBrk,
			PushBreaker:    pushBrk,
			Links: hal.LinkMap{
				"self": {Href: originSelfURL(b, repoName)},
				"repo": {Href: b.Repo(repoName)},
			},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleHALSetOrigin serves PUT /repos/{repo}/origin.
//
// The request is resolved against the stored origin (an empty field keeps
// what is stored) and validated here; the attach itself is the lifecycle
// event AttachOrigin. Its guard probes the remote under the network timeout
// BEFORE anything is stopped or written: an unreachable remote or a refused
// credential is a 502 with NOTHING persisted and the running sync loop
// untouched, a different knowledge base a 409. While the repo is indexing the
// attach is refused with 409 and a cancel-index link. On success the Sync
// stage has been restarted on the new origin when this answers 200.
func handleHALSetOrigin(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		var req setOriginRequest
		if !httpx.DecodeJSON(w, r, &req, 0) {
			return
		}
		// Before every read of req.URL below, so the policy gate, the probe
		// and the write all see the same string. A url of only spaces stays
		// the partial-update case (reuse what is stored).
		req.URL = httpx.TrimOriginURL(req.URL)

		// The local-origin policy at the write edge, before anything else
		// reads req.URL (the attach guard re-asserts it). A partial update
		// (empty url) reuses the stored URL, gated when it was first written.
		if req.URL != "" {
			if err := m.ValidateLocalOrigin(req.URL); err != nil {
				hal.WriteProblem(w, http.StatusBadRequest, "Origin not allowed",
					detailWithoutTitlePrefix(err, repos.ErrLocalOriginDenied), r.URL.Path)
				return
			}
		}

		existing, err := getOrigin(r.Context(), ri)
		if httpx.StoreUnavailable(err) {
			httpx.WriteStoreUnavailable(w, r)
			return
		}
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to get origin", err.Error(), r.URL.Path)
			return
		}
		u := req.URL
		if u == "" && existing != nil {
			u = existing.URL
		}
		if u == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "URL required", errOriginURLRequired.Error(), r.URL.Path)
			return
		}
		if req.URL != "" && !httpx.IsGitURL(req.URL) {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid URL", errOriginInvalidURL.Error(), r.URL.Path)
			return
		}
		authMethod := req.AuthMethod
		// A stored cert is knomit's decision, not an input: the attach
		// decides again (repos.Manager.ResolveOrigin), so it is not carried.
		if authMethod == "" && existing != nil && existing.AuthMethod != "cert" {
			authMethod = existing.AuthMethod
		}
		authToken := httpx.AssembleAuthToken(authMethod, req.Token, req.User, req.Password)
		if authToken == "" && existing != nil {
			authToken = existing.AuthToken
		}
		if verr := httpx.ValidateURLAuth(u, authMethod); verr != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid origin", verr.Error(), r.URL.Path)
			return
		}

		spec := repos.OriginSpec{
			URL:        u,
			Branch:     req.Branch,
			AuthMethod: authMethod,
			AuthToken:  authToken,
		}
		attach := repos.AttachOrigin
		if req.ReplaceMain {
			attach = repos.AttachOriginReplacing
		}
		_, err = m.Send(r.Context(), ri, attach(spec))
		if err != nil {
			writeAttachRefusal(w, r, b, repoName, err, "Failed to set origin")
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
	// ReplaceMain is PUT /origin's replace_main for the new branch: admit a
	// branch that shares no history with the current consensus branch.
	ReplaceMain bool `json:"replace_main,omitempty"`
}

// writeAttachRefusal maps an attach guard's refusal (PUT /origin's
// AttachOrigin, PATCH /origin/upstream's CheckOriginBranch) onto its problem
// document; anything it does not name is a 500 titled fallback.
func writeAttachRefusal(w http.ResponseWriter, r *http.Request, b hal.URLBuilder, repoName string, err error, fallback string) {
	switch {
	case httpx.IsLifecycleRefusal(err):
		httpx.WriteLifecycleProblem(w, r, b, repoName, err)
	case errors.Is(err, repos.ErrLocalOriginDenied):
		hal.WriteProblem(w, http.StatusBadRequest, "Origin not allowed",
			detailWithoutTitlePrefix(err, repos.ErrLocalOriginDenied), r.URL.Path)
	case errors.Is(err, repos.ErrPeerCredential), errors.Is(err, repos.ErrCertNotAnInput), errors.Is(err, pki.ErrUnsupportedOriginScheme):
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid origin", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrOriginOntologyConflict):
		hal.WriteProblem(w, http.StatusConflict, "Different knowledge base", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrKnowledgeBaseAlreadyLocal):
		hal.WriteProblem(w, http.StatusConflict, "Knowledge base already local", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrOriginUnrelatedHistory):
		hal.WriteProblem(w, http.StatusConflict, "Unrelated history", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrOriginNotInspected):
		hal.WriteProblem(w, http.StatusConflict, "Origin not inspected", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrRegistryUnavailable):
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Registry unavailable", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrOriginUnreachable):
		log.Warn().Err(err).Str("repo", repoName).Msg("origin attach refused by its probe")
		hal.WriteProblem(w, http.StatusBadGateway, "Origin not attached", err.Error(), r.URL.Path)
	case errors.Is(err, repos.ErrManagerStopped):
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Server stopping", err.Error(), r.URL.Path)
	case httpx.StoreUnavailable(err):
		httpx.WriteStoreUnavailable(w, r)
	default:
		hal.WriteProblem(w, http.StatusInternalServerError, fallback, err.Error(), r.URL.Path)
	}
}

// writeNoOrigin answers an upstream change on a repo with no origin: 409,
// because the request conflicts with the repo's state (attach one first).
func writeNoOrigin(w http.ResponseWriter, r *http.Request) {
	hal.WriteProblem(w, http.StatusConflict, "No origin",
		"this repo has no origin, so there is no upstream branch to change; attach one with PUT /origin",
		r.URL.Path)
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
func handleHALSetOriginUpstream(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		var req upstreamRequest
		if !httpx.DecodeJSON(w, r, &req, 0) {
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
		// A stopped manager is refused first, as setOriginUpstream would,
		// so its answer stays the one the route documents.
		if _, err := originsOf(m); err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to set upstream branch",
				err.Error(), r.URL.Path)
			return
		}
		// The new branch is a different consensus branch, so it is vetted
		// like an attach (CheckOriginBranch: another repo's knowledge base,
		// unrelated history) before the refspec moves. A guard only, not the
		// AttachOrigin event: no indexing exclusivity, no Sync restart.
		// FAIL CLOSED: an origin that cannot be read, or no origin at all,
		// is refused here; nothing is ever re-pointed without the check.
		existing, gerr := getOrigin(r.Context(), ri)
		switch {
		case httpx.StoreUnavailable(gerr):
			httpx.WriteStoreUnavailable(w, r)
			return
		case gerr != nil:
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to get origin", gerr.Error(), r.URL.Path)
			return
		case existing == nil || existing.URL == "":
			writeNoOrigin(w, r)
			return
		}
		authMethod := existing.AuthMethod
		if authMethod == "cert" {
			// knomit's decision, not an input: the guard decides again,
			// exactly as PUT /origin does.
			authMethod = ""
		}
		if err := m.CheckOriginBranch(r.Context(), ri, repos.OriginSpec{
			URL:        existing.URL,
			Branch:     req.Branch,
			AuthMethod: authMethod,
			AuthToken:  existing.AuthToken,
		}, req.ReplaceMain); err != nil {
			writeAttachRefusal(w, r, b, repoName, err, "Failed to set upstream branch")
			return
		}
		// Applied only to the URL just vetted (a compare-and-set): an origin
		// re-pointed or removed since the check is refused, not re-pointed.
		if err := setOriginUpstream(r.Context(), m, ri, existing.URL, req.Branch); err != nil {
			switch {
			case httpx.StoreUnavailable(err):
				httpx.WriteStoreUnavailable(w, r)
			case errors.Is(err, errNoOrigin):
				writeNoOrigin(w, r)
			case errors.Is(err, control.ErrOriginChanged):
				hal.WriteProblem(w, http.StatusConflict, "Origin changed",
					"the origin was changed while the new branch was being checked; nothing was changed, so read the origin and retry",
					r.URL.Path)
			default:
				hal.WriteProblem(w, http.StatusInternalServerError, "Failed to set upstream branch",
					err.Error(), r.URL.Path)
			}
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
//
// It sends DetachOrigin: the Sync stage is exited, the git remote, the
// control.db record and the injected origin are removed, and Sync re-enters
// with the origin-less local loop, so the consensus branch keeps following
// the agent branch at once. A subscription IS its origin and is refused (409),
// as is a repo that is indexing (409 with a cancel-index link). Returns 204.
func handleHALDeleteOrigin(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		_, err := m.Send(r.Context(), ri, repos.DetachOrigin())
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, repos.ErrSubscriptionOrigin):
			hal.WriteProblem(w, http.StatusConflict, "Subscription requires its origin",
				"this repo follows its origin read-only and has no content of its own; archive it instead of detaching the origin",
				r.URL.Path)
		case httpx.IsLifecycleRefusal(err):
			httpx.WriteLifecycleProblem(w, r, b, chi.URLParam(r, "repo"), err)
		default:
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to delete origin",
				err.Error(), r.URL.Path)
		}
	}
}
