package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/engine/store/merge"
	"knomit/internal/engine/store/remotesync"
	"knomit/internal/platform/fileuri"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// A switch to a different origin is synced by the ordinary merge path, never
// "rewound" (user ruling 2026-10-18, #544; origin_identity.go). Every test runs
// the whole Sync and Push against real bare remotes. The fixture: this copy
// synced with A (main r) and consumed its own commit f (pushed, taken into
// A's main). B is a fork of A taken at r, so B's main is BEHIND local main.

func swCloneBare(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "b.git")
	mustRun(t, t.TempDir(), "git", "clone", "--bare", "--quiet", src, dst)
	return dst
}

func swBareHasFile(t *testing.T, bare, branch, path string) bool {
	t.Helper()
	c := exec.Command("git", "cat-file", "-e", "refs/heads/"+branch+":"+path)
	c.Dir = bare
	return c.Run() == nil
}

func swBareHolds(t *testing.T, bare, branch string, h plumbing.Hash) bool {
	t.Helper()
	c := exec.Command("git", "merge-base", "--is-ancestor", h.String(), "refs/heads/"+branch)
	c.Dir = bare
	return c.Run() == nil
}

// swCommitOnBareMain commits path on the bare remote's main with the git CLI.
func swCommitOnBareMain(t *testing.T, bare, path, body string) {
	t.Helper()
	work := t.TempDir()
	mustRun(t, "", "git", "clone", "--quiet", bare, work)
	mustRun(t, work, "git", "checkout", "-q", "main")
	mustRun(t, work, "git", "config", "user.email", "o@o")
	mustRun(t, work, "git", "config", "user.name", "other")
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(work, path)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, path), []byte(body), 0o644))
	mustRun(t, work, "git", "add", path)
	mustRun(t, work, "git", "commit", "-q", "-m", "commit "+path)
	mustRun(t, work, "git", "push", "-q", "origin", "main")
}

// swConsumeOwn writes kb/own.md on the agent branch, pushes it, has the origin
// take it into main, and syncs: this copy consumed its own commit f.
func swConsumeOwn(t *testing.T, svc *Service, bare string) plumbing.Hash {
	t.Helper()
	f := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/own.md", "Own", "consumed by A"))
	wmPush(t, svc)
	wmSetOriginMain(t, bare, f)
	wmSync(t, svc)
	require.Equal(t, f, mustHeadHash(t, svc, "main"), "setup: this copy consumed f")
	return f
}

// swPoint points the repository at url the way an attach does.
func swPoint(t *testing.T, svc *Service, url string) {
	t.Helper()
	require.NoError(t, svc.ConfigureRemote(url, "main", "agent/test"))
	svc.Remote().SetOrigin(&remotesync.Origin{URL: url, Branch: "main"})
}

func swTrySync(svc *Service) (remotesync.SyncResult, error) {
	return svc.Remote().Sync(context.Background(), "agent/test", nil)
}

// A switch to an origin whose main is BEHIND local main: nothing is dropped
// from the branch (not f, not the unsynced w), and the branch is pushed to
// B. At origin/dev the first sync with B rewound and dropped f.
// SABOTAGE: rewind on a switch (no switch rule) -> f dropped -> red.
func TestSync_SwitchToAnOriginBehindDropsNothing(t *testing.T) {
	for _, detach := range []bool{false, true} {
		name := "re-pointed"
		if detach {
			name = "detached then attached"
		}
		t.Run(name, func(t *testing.T) {
			svc, a, _ := wmFixture(t)
			b := swCloneBare(t, a)
			f := swConsumeOwn(t, svc, a)
			w := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "own, unsynced"))
			if detach {
				require.NoError(t, svc.Remote().DeleteRemote("origin"))
				svc.Remote().SetOrigin(nil)
			}

			swPoint(t, svc, fileuri.New(b))
			res := wmSync(t, svc)
			require.NotEqual(t, merge.ModeRebase, res.Agent.Mode, "synced normally, not rebuilt")
			require.True(t, reachableFrom(t, svc, "agent/test", f), "f is kept as it is")
			require.True(t, reachableFrom(t, svc, "agent/test", w), "w is kept as it is")
			wmPush(t, svc)
			require.True(t, swBareHolds(t, b, "agent/test", f), "f reaches B")
			require.True(t, swBareHolds(t, b, "agent/test", w), "w reaches B")
			id, err := svc.rr.ReadOriginIdentityForTest()
			require.NoError(t, err)
			require.Equal(t, remotesync.OriginIdentityForTest(fileuri.New(b)), id, "B's identity is recorded")
		})
	}
}

// A switch to an origin that DIVERGED (it has a commit of its own): merged
// normally, nothing dropped, own work pushed.
// SABOTAGE: rewind on a switch -> f dropped -> red.
func TestSync_SwitchToADivergedOriginMergesNormally(t *testing.T) {
	svc, a, _ := wmFixture(t)
	b := swCloneBare(t, a)
	f := swConsumeOwn(t, svc, a)
	w := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "own, unsynced"))
	swCommitOnBareMain(t, b, "kb/b-own.md", "B's own main")

	swPoint(t, svc, fileuri.New(b))
	res := wmSync(t, svc)
	require.Equal(t, merge.ModeRewound, res.Main.Mode, "setup: local main moved to B's")
	require.NotEqual(t, merge.ModeRebase, res.Agent.Mode)
	require.True(t, reachableFrom(t, svc, "agent/test", f))
	require.True(t, reachableFrom(t, svc, "agent/test", w))
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/b-own.md"), "B's main is merged in")
	wmPush(t, svc)
	require.True(t, swBareHolds(t, b, "agent/test", f))
	require.True(t, swBareHolds(t, b, "agent/test", w))
}

// A switch to an UNRELATED origin (no common commit) keeps the existing
// rules, unchanged from origin/dev (issue #541 will handle it): the branch
// stays as it stands.
// SABOTAGE: take the merge path for unrelated histories too -> the branch is
// merged with B's unrelated main -> red.
func TestSync_SwitchToAnUnrelatedOriginKeepsTheExistingRules(t *testing.T) {
	svc, a, _ := wmFixture(t)
	swConsumeOwn(t, svc, a)
	tip := mustHeadHash(t, svc, "agent/test")
	b := t.TempDir()
	mustRun(t, "", "git", "init", "-q", "--bare", "--initial-branch=main", b)
	work := t.TempDir()
	mustRun(t, "", "git", "init", "-q", "--initial-branch=main", work)
	mustRun(t, work, "git", "config", "user.email", "u@u")
	mustRun(t, work, "git", "config", "user.name", "unrelated")
	require.NoError(t, os.WriteFile(filepath.Join(work, "unrelated.txt"), []byte("an unrelated history"), 0o644))
	mustRun(t, work, "git", "add", "unrelated.txt")
	mustRun(t, work, "git", "commit", "-q", "-m", "unrelated root")
	mustRun(t, work, "git", "push", "-q", b, "main")

	swPoint(t, svc, fileuri.New(b))
	res := wmSync(t, svc)
	require.Equal(t, merge.ModeNoop, res.Agent.Mode, "the existing disjoint rule: the branch stays as it is")
	require.Equal(t, tip, mustHeadHash(t, svc, "agent/test"))
}

// A first round with B that fails before its reconcile does not record B, so
// the next round is still a switch and converges to the same end state.
// SABOTAGE: record the identity before the reconcile -> round 2 is not a
// switch, rewinds and drops f -> red.
func TestSync_FailedFirstRoundWithANewOriginIsStillASwitch(t *testing.T) {
	svc, a, _ := wmFixture(t)
	b := swCloneBare(t, a)
	f := swConsumeOwn(t, svc, a)

	failed := errors.New("seam: crash after the fetch")
	once := false
	setStoreHooks(t, storeHooks{afterFetch: func() error {
		if once {
			return nil
		}
		once = true
		return failed
	}})
	swPoint(t, svc, fileuri.New(b))
	_, err := swTrySync(svc)
	require.ErrorIs(t, err, failed)
	id, err := svc.rr.ReadOriginIdentityForTest()
	require.NoError(t, err)
	require.Equal(t, remotesync.OriginIdentityForTest(fileuri.New(a)), id, "a failed round records nothing")

	wmSync(t, svc)
	require.True(t, reachableFrom(t, svc, "agent/test", f), "f is kept")
	wmPush(t, svc)
	require.True(t, swBareHolds(t, b, "agent/test", f))
}

// The SAME origin moving its main back keeps the existing rewind rules: the
// commit it removed is not resurrected.
// SABOTAGE: treat every round as a switch -> f kept -> red.
func TestSync_SameOriginMovedBackStillRewinds(t *testing.T) {
	svc, a, r := wmFixture(t)
	f := swConsumeOwn(t, svc, a)
	wmSetOriginMain(t, a, r)
	res := wmSync(t, svc)
	require.Equal(t, merge.ModeRewound, res.Main.Mode)
	require.False(t, reachableFrom(t, svc, "agent/test", f), "the removed f stays removed")
	wmPush(t, svc)
	require.False(t, swBareHasFile(t, a, "agent/test", "kb/own.md"), "not pushed back")
}

// Another spelling or a credential change of the same origin is NOT a switch:
// a later scrub on that origin still rewinds. And no credential is ever
// recorded.
// SABOTAGE: compare the raw URL -> treated as a switch, f kept -> red.
func TestSync_AnotherSpellingOrCredentialIsNotASwitch(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  func(a string) string
	}{
		{"credential in the URL", func(a string) string {
			return "file://user:s3cret@" + strings.TrimPrefix(fileuri.New(a), "file://") + "/"
		}},
		{"another spelling", func(a string) string {
			u := fileuri.New(a)
			i := strings.LastIndex(u, "/")
			return u[:i] + "/." + u[i:] + ".git/"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, a, r := wmFixture(t)
			f := swConsumeOwn(t, svc, a)
			svc.Remote().SetOrigin(&remotesync.Origin{URL: tc.url(a), Branch: "main"}) // the git remote stays A
			wmSetOriginMain(t, a, r)
			wmSync(t, svc)
			require.False(t, reachableFrom(t, svc, "agent/test", f), "the same origin's scrub still holds")
			id, err := svc.rr.ReadOriginIdentityForTest()
			require.NoError(t, err)
			require.NotContains(t, id, "s3cret", "no credential is recorded")
			require.NotContains(t, id, "user", "no user info is recorded")
		})
	}
}

// B already holds f in main but has no agent branch: the tracking ref for the
// agent branch, left from A, used to make Push report "nothing to push". A
// fetch from an origin without the branch now drops it.
// SABOTAGE: keep the tracking ref when the origin lacks the branch -> red.
func TestSync_SwitchPushesTheBranchTheNewOriginLacks(t *testing.T) {
	svc, a, _ := wmFixture(t)
	swConsumeOwn(t, svc, a)
	b := swCloneBare(t, a)
	mustRun(t, b, "git", "update-ref", "-d", "refs/heads/agent/test")

	swPoint(t, svc, fileuri.New(b))
	wmSync(t, svc)
	_, err := svc.rh.Storer().Reference(plumbing.NewRemoteReferenceName("origin", "agent/test"))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "the tracking ref mirrors B, which has no agent branch")
	pres, err := svc.Remote().Push(context.Background(), "agent/test", nil)
	require.NoError(t, err)
	require.True(t, pres.Pushed)
	require.True(t, swBareHasFile(t, b, "agent/test", "kb/own.md"))
}
