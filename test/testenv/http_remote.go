package testenv

import (
	"path/filepath"

	"knomit/test/testenv/gitserver"
)

// BareRemoteHTTP creates a bare git remote exactly like
// BareRemoteWithBranch(name, "main") — same on-disk layout at
// <storyboard-tempdir>/remotes/<name>, same "main" symbolic HEAD — but
// exposes it over a real smart-HTTP git server (test/testenv/gitserver)
// instead of a file:// URL. The returned RemoteHandle is a drop-in for the
// file:// variant: every mutator (MergeIntoMain, WriteMain, Push, …) still
// operates on r.dir directly, so only the transport seen by the PRODUCT
// (RepoHandle.Connect → InitFromRemote clone, BranchHandle.Push) changes.
//
// This is the keystone wiring that lets scenario tests drive the real
// product clone/push path over a fault-injectable HTTP endpoint. With no
// faults configured the server behaves like a vanilla git host; inject
// failures via the FaultPlan returned by RemoteHandle.Fault().
//
// Push over smart-HTTP requires http.receivepack=true on the bare repo
// (git refuses receive-pack over HTTP otherwise), so this helper enables
// it. The server is rooted at the parent remotes/ directory so the repo is
// addressable by name (<server-URL>/<name>), and it is torn down via
// t.Cleanup on test completion.
func (sb *Storyboard) BareRemoteHTTP(name string) *RemoteHandle {
	t := sb.t
	t.Helper()

	// Build the bare repo identically to the file:// variant.
	r := sb.BareRemoteWithBranch(name, "main")

	// Allow push (receive-pack) over smart HTTP; git rejects it by default.
	mustGit(t, "", "--git-dir="+r.dir, "config", "http.receivepack", "true")

	// Serve the parent remotes/ directory so <URL>/<name> resolves to this
	// bare repo. All remotes created on this Storyboard live under the same
	// parent, so a single server per remote is harmless overlap; each gets
	// its own FaultPlan.
	srv := gitserver.New(t, filepath.Join(sb.homeDir, "remotes"))
	t.Cleanup(srv.Close)

	r.url = srv.URL + "/" + name
	r.httpSrv = srv
	return r
}

// BareRemoteHTTPWithBranch is BareRemoteHTTP parameterized on the upstream
// (default) branch name. It builds the bare repo exactly like
// BareRemoteWithBranch(name, branch) — so the bare repo's symbolic HEAD points
// at refs/heads/<branch> — then serves it over the same fault-injectable
// smart-HTTP server BareRemoteHTTP uses. Use this to drive the product clone
// path against a remote whose default branch is e.g. "master" (no "main"),
// exercising the upstream-detection fallback over a real HTTP transport.
func (sb *Storyboard) BareRemoteHTTPWithBranch(name, branch string) *RemoteHandle {
	t := sb.t
	t.Helper()
	if branch == "" {
		branch = "main"
	}

	// Build the bare repo with the requested symbolic HEAD.
	r := sb.BareRemoteWithBranch(name, branch)

	// Allow push (receive-pack) over smart HTTP; git rejects it by default.
	mustGit(t, "", "--git-dir="+r.dir, "config", "http.receivepack", "true")

	srv := gitserver.New(t, filepath.Join(sb.homeDir, "remotes"))
	t.Cleanup(srv.Close)

	r.url = srv.URL + "/" + name
	r.httpSrv = srv
	return r
}

// Fault returns the live FaultPlan for an HTTP-served remote, or nil for a
// file:// remote. Tests mutate the returned plan to inject transport
// failures (status codes, hangs, truncation, throttling, auth, expiry) into
// the product's clone/fetch/push path.
func (r *RemoteHandle) Fault() *gitserver.FaultPlan {
	if r.httpSrv == nil {
		return nil
	}
	return r.httpSrv.Fault()
}
