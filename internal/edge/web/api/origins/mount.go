// Package origins serves the origin-session workflow: the connect-a-remote
// wizard under /repos/{repo}/origin-sessions (create, test, preview, apply,
// commit). Its only export is Mount; the session manager, its sessions and
// the handlers are private to it.
package origins

import (
	"context"
	"sync"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
)

// Mount registers the origin-session routes on r, which the caller mounts at
// /repos/{repo}/origin-sessions INSIDE the RepoMiddleware group: every handler
// reads the repo with repos.RepoFromContext. rm is the repo manager the test
// and commit steps act through, and agentBranch the instance's agent branch.
//
// Mount creates the session manager its routes share and owns it, for as
// long as ctx lives: when ctx ends, every session and its folder is removed
// (the LIFETIME note on sessionManager). sessionsDir is the directory the
// manager keeps its run, and its session folders, in. Several processes may
// share one (two servers on one home): each manager's run is guarded by an OS
// lock it holds for its life, and Mount starts a background sweep that removes
// only the runs whose lock is free, i.e. whose process has exited (FOLDERS on
// sessionManager). "" gives the manager a private directory under
// os.TempDir() instead.
//
// That removal runs after ctx ends, asynchronously. A non-nil released is
// how the caller waits for it: Mount adds one to it, and the manager marks it
// Done once ctx has ended and it has let go of everything it held (every
// session's store closed and folder removed, its run directory and lock file
// removed with the lock's handle closed, its startup sweep finished). A
// caller that deletes sessionsDir (or the temp dir a "" manager uses) waits
// on it first: on Windows a file with an open handle cannot be removed. While
// ctx is live nothing runs on released's behalf (no goroutine waits on it).
//
// Two Mount calls make two managers that know nothing of each other's
// sessions, so a caller that serves the routes from more than one router
// mounts ONE r in all of them (web's router does, for its plain and OAuth
// listeners).
func Mount(ctx context.Context, r chi.Router, rm *repos.Manager, agentBranch, sessionsDir string, released *sync.WaitGroup) {
	sm := newSessionManager(ctx, sessionsDir)
	if released != nil {
		released.Add(1)
		// close is once-only and returns when the first call has finished,
		// whichever of the two AfterFuncs runs it.
		context.AfterFunc(ctx, func() {
			sm.close()
			released.Done()
		})
	}
	routes(r, rm, agentBranch, sm)
}

// routes is Mount's route table, over a given manager so the package's tests
// can reach the sessions their requests create.
func routes(r chi.Router, rm *repos.Manager, agentBranch string, sm *sessionManager) {
	b := hal.URLBuilder{Base: httpx.APIBase}
	r.Get("/", handleListSessions(b, sm))
	r.Post("/", handleCreateSession(b, sm))
	r.Get("/{sessionID}", handleGetSession(b, sm))
	r.Delete("/{sessionID}", handleDeleteSession(sm))
	r.Get("/{sessionID}/test", handleTestConnectivity(rm, sm, agentBranch))
	r.Get("/{sessionID}/preview", handlePreview(sm, agentBranch))
	r.Post("/{sessionID}/apply", handleApply(sm, agentBranch))
	r.Post("/{sessionID}/commit", handleCommit(rm, sm, agentBranch))
}
