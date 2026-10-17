package store

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"knomit/internal/platform/fileuri"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// When a repository is reconnected to a DIFFERENT origin URL, the position it
// remembers (the watermark and the remote-tracking refs) belongs to the old
// origin. Read against the new one, own commits the old origin had taken but
// the new one lacks were below the watermark and dropped on the first sync,
// and a stale refs/remotes/origin/<agent branch> made Push send nothing. Sync
// now forgets the old origin's position at the first sync with a new URL
// (origin_position.go), so that sync behaves as a first sync. These tests run
// the whole Sync and Push against real bare remotes.

// ocCloneBare makes a bare copy of the bare repository src (all its branches)
// and returns its path.
func ocCloneBare(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "b.git")
	mustRun(t, t.TempDir(), "git", "clone", "--bare", "--quiet", src, dst)
	return dst
}

// ocBareHasFile reports whether the bare remote's branch has path.
func ocBareHasFile(t *testing.T, bare, branch, path string) bool {
	t.Helper()
	c := exec.Command("git", "cat-file", "-e", "refs/heads/"+branch+":"+path)
	c.Dir = bare
	return c.Run() == nil
}

// ocConsumeOwn writes kb/own.md on the agent branch, pushes it to the origin
// at bare, has that origin take it into main and syncs: this copy consumed
// its own commit f (main and the watermark are f). Returns f.
func ocConsumeOwn(t *testing.T, svc *Service, bare string) plumbing.Hash {
	t.Helper()
	f := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/own.md", "Own", "consumed by the old origin"))
	wmPush(t, svc)
	wmSetOriginMain(t, bare, f)
	wmSync(t, svc)
	require.Equal(t, f, mustHeadHash(t, svc, "main"), "setup: this copy consumed f")
	base, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, f, base, "setup: the watermark is f")
	return f
}

// ocRepoint points the repository at url the way an attach does: the git
// remote first, then the injected origin.
func ocRepoint(t *testing.T, svc *Service, url string) {
	t.Helper()
	require.NoError(t, svc.ConfigureRemote(url, "main", "agent/test"))
	svc.SetOrigin(&Origin{URL: url, Branch: "main"})
}

// Reconnect A -> B, where B is a fork of A taken BEFORE A consumed this copy's
// own commit f: B's main is r and B has no agent branch. f is kept and pushed
// to B. At origin/dev the watermark f made the replay drop f.
// SABOTAGE: never forget (originPositionStale always false) -> f dropped -> red.
func TestSync_OriginURLChangeKeepsAndPushesOwnCommitTheNewOriginLacks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		repoint func(t *testing.T, svc *Service, url string)
	}{
		{"re-pointed in place", ocRepoint},
		{"detached then attached", func(t *testing.T, svc *Service, url string) {
			require.NoError(t, svc.Remote().DeleteRemote("origin"))
			svc.SetOrigin(nil)
			ocRepoint(t, svc, url)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, a, r := wmFixture(t)
			b := ocCloneBare(t, a) // B: main r, no agent branch
			f := ocConsumeOwn(t, svc, a)
			require.False(t, ocBareHasFile(t, b, "main", "kb/own.md"), "setup: B lacks f")

			tc.repoint(t, svc, fileuri.New(b))
			res := wmSync(t, svc)
			require.Equal(t, ModeRewound, res.Main.Mode, "B's main r is behind local main f")
			require.Equal(t, r, mustHeadHash(t, svc, "main"))
			require.True(t, hasFileAt(t, svc, "agent/test", "kb/own.md"), "f is kept")
			require.True(t, reachableFrom(t, svc, "agent/test", f), "f is kept as it is")

			pres, err := svc.Remote().(*remoteIndex).Push(context.Background(), "agent/test", nil)
			require.NoError(t, err)
			require.True(t, pres.Pushed, "the branch goes to B")
			require.True(t, ocBareHasFile(t, b, "agent/test", "kb/own.md"), "f reaches B")
		})
	}
}

// The first sync after a forget behaves as a first sync: with no watermark and
// no tracking ref nothing is seeded, the branch is not rebuilt (its hashes stay
// as they are, the merge-base fallback, as TestSync_FirstSyncEverKeepsTheMerge
// BaseFallback), the old origin's tracking refs are gone, and the reconcile
// writes the watermark at the new origin's main.
// SABOTAGE: forget the watermarks but NOT the tracking refs -> lastOrigin is
// A's f, the seed is f, the replay drops f -> red.
func TestSync_FirstSyncAfterAnOriginChangeIsAFirstSync(t *testing.T) {
	svc, a, r := wmFixture(t)
	b := ocCloneBare(t, a)
	f := ocConsumeOwn(t, svc, a)

	ocRepoint(t, svc, fileuri.New(b))
	res := wmSync(t, svc)
	require.Equal(t, ModeNoop, res.Agent.Mode, "nothing seeded: the branch reads as ahead and is kept")
	require.Equal(t, f, mustHeadHash(t, svc, "agent/test"), "not rebuilt")
	_, err := svc.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", "agent/test"))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "A's tracking ref for the agent branch is forgotten (B has none)")
	tr, err := svc.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", "main"))
	require.NoError(t, err)
	require.Equal(t, r, tr.Hash(), "origin/main is B's")
	base, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, r, base, "the reconcile writes a true watermark for B")
	url, err := svc.rh.readOriginPositionURL()
	require.NoError(t, err)
	require.Equal(t, fileuri.New(b), url, "the position is now B's")

	// The next sync with B forgets nothing: same URL.
	f2 := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/later.md", "Later", "after the move"))
	wmSync(t, svc)
	require.True(t, reachableFrom(t, svc, "agent/test", f2))
	base, err = svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, r, base, "the watermark stays B's")
}

// B is a mirror that already holds f in main but has no agent branch. Nothing
// is rewound and nothing changes on the branch, so at origin/dev A's stale
// refs/remotes/origin/agent/test equalled the branch tip and Push sent
// nothing: B never got the branch. Forgotten, the push goes out.
// SABOTAGE: forget the watermarks but NOT the tracking refs -> Pushed=false -> red.
func TestSync_OriginURLChangePushesTheBranchTheNewOriginLacks(t *testing.T) {
	svc, a, _ := wmFixture(t)
	f := ocConsumeOwn(t, svc, a)
	b := ocCloneBare(t, a)
	mustRun(t, b, "git", "update-ref", "-d", "refs/heads/agent/test")
	require.True(t, wmOriginHolds(t, b, "main", f), "setup: B holds f in main")

	ocRepoint(t, svc, fileuri.New(b))
	wmSync(t, svc)
	require.Equal(t, f, mustHeadHash(t, svc, "agent/test"))
	pres, err := svc.Remote().(*remoteIndex).Push(context.Background(), "agent/test", nil)
	require.NoError(t, err)
	require.True(t, pres.Pushed, "B lacks the agent branch, so it is pushed")
	require.True(t, ocBareHasFile(t, b, "agent/test", "kb/own.md"))
}

// A credential change keeps the URL, so it keeps the position: an origin that
// then removes f (resets main to r, an ancestor of the branch) still has f
// removed from this copy, as on any same-origin scrub.
// SABOTAGE: forget on every SetOrigin / compare the whole origin (credential
// included) -> f kept -> red.
func TestSync_CredentialChangeKeepsTheOriginPosition(t *testing.T) {
	svc, a, r := wmFixture(t)
	f := ocConsumeOwn(t, svc, a)

	url := fileuri.New(a)
	require.NoError(t, svc.ConfigureRemote(url, "main", "agent/test"))
	svc.SetOrigin(&Origin{URL: url, Branch: "main", AuthMethod: "basic", AuthToken: "new-secret"})
	base, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, f, base, "the watermark is kept")

	wmSetOriginMain(t, a, r)
	res := wmSync(t, svc)
	require.Equal(t, ModeRewound, res.Main.Mode)
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/own.md"), "the removed commit stays removed")
	require.False(t, reachableFrom(t, svc, "agent/test", f))
}

// A commit the SAME origin removed is still not resurrected: nothing changed
// but the origin's main, so nothing is forgotten.
// SABOTAGE: forget on every Sync -> f kept -> red.
func TestSync_SameOriginRemovedCommitIsNotResurrected(t *testing.T) {
	svc, a, r := wmFixture(t)
	f := ocConsumeOwn(t, svc, a)

	wmSetOriginMain(t, a, r)
	res := wmSync(t, svc)
	require.Equal(t, ModeRewound, res.Main.Mode)
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/own.md"))
	require.False(t, reachableFrom(t, svc, "agent/test", f))
	wmPush(t, svc)
	require.False(t, wmOriginHolds(t, a, "agent/test", f), "the push does not send it back")
}

// A repository made before the position was tagged: SetOrigin with its stored
// origin (what every open does) adopts that URL, so a change of URL made
// before its first Sync is still seen as a change.
// SABOTAGE: drop the adopt from SetOrigin -> the first Sync adopts B -> f
// dropped -> red.
func TestSync_UntaggedRepositoryAdoptsItsOriginAtOpen(t *testing.T) {
	svc, a, _ := wmFixture(t)
	b := ocCloneBare(t, a)
	f := ocConsumeOwn(t, svc, a)
	_, err := svc.rh.db.Exec(`DELETE FROM meta WHERE key = ?`, originPositionURLKey)
	require.NoError(t, err)

	svc.SetOrigin(&Origin{URL: fileuri.New(a), Branch: "main"}) // the open
	url, err := svc.rh.readOriginPositionURL()
	require.NoError(t, err)
	require.Equal(t, fileuri.New(a), url, "adopted at open")

	ocRepoint(t, svc, fileuri.New(b))
	wmSync(t, svc)
	require.True(t, reachableFrom(t, svc, "agent/test", f), "f is kept")
}
