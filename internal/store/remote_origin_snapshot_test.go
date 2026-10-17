package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/platform/fileuri"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// Snapshot-then-reconcile across a change of origin (origin_snapshot.go).
// Every test runs the whole Sync and Push against real bare remotes. The
// fixture: this copy synced with A (main r), consumed its own commit f
// (pushed, taken into A's main), then took a foreign commit x that someone
// else put on A's main. B is a fork of A taken at r.

// ── helpers ──────────────────────────────────────────────────────────────

func ocCloneBare(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "b.git")
	mustRun(t, t.TempDir(), "git", "clone", "--bare", "--quiet", src, dst)
	return dst
}

func ocBareHasFile(t *testing.T, bare, branch, path string) bool {
	t.Helper()
	c := exec.Command("git", "cat-file", "-e", "refs/heads/"+branch+":"+path)
	c.Dir = bare
	return c.Run() == nil
}

// ocForeignOnBareMain commits path on the bare remote's main with the git CLI
// (an unsigned commit by someone else) and returns its hash.
func ocForeignOnBareMain(t *testing.T, bare, path, body string) plumbing.Hash {
	t.Helper()
	work := t.TempDir()
	mustRun(t, "", "git", "clone", "--quiet", bare, work)
	mustRun(t, work, "git", "checkout", "-q", "main")
	mustRun(t, work, "git", "config", "user.email", "o@o")
	mustRun(t, work, "git", "config", "user.name", "someone-else")
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(work, path)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, path), []byte(body), 0o644))
	mustRun(t, work, "git", "add", path)
	mustRun(t, work, "git", "commit", "-q", "-m", "foreign "+path)
	mustRun(t, work, "git", "push", "-q", "origin", "main")
	c := exec.Command("git", "rev-parse", "HEAD")
	c.Dir = work
	out, err := c.Output()
	require.NoError(t, err)
	return plumbing.NewHash(strings.TrimSpace(string(out)))
}

// ocConsumeOwn writes kb/own.md on the agent branch, pushes it, has the
// origin take it into main, and syncs: this copy consumed its own f.
func ocConsumeOwn(t *testing.T, svc *Service, bare string) plumbing.Hash {
	t.Helper()
	f := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/own.md", "Own", "consumed by A"))
	wmPush(t, svc)
	wmSetOriginMain(t, bare, f)
	wmSync(t, svc)
	require.Equal(t, f, mustHeadHash(t, svc, "main"), "setup: this copy consumed f")
	return f
}

// ocRepoint points the repository at url the way an attach does.
func ocRepoint(t *testing.T, svc *Service, url string) {
	t.Helper()
	require.NoError(t, svc.ConfigureRemote(url, "main", "agent/test"))
	svc.SetOrigin(&Origin{URL: url, Branch: "main"})
}

func ocKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	k, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	return k
}

// ocUnsignedWrite commits path on branch WITHOUT a signature (a write made
// before signing was enforced) and moves the branch to it.
func ocUnsignedWrite(t *testing.T, svc *Service, branch, path, body string) plumbing.Hash {
	t.Helper()
	ref, err := svc.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	require.NoError(t, err)
	sig := object.Signature{Name: "me", Email: "me@local", When: time.Now()}
	h, _, err := writeFileToStore(svc.rh.gits, nil, ref.Hash(), path, obsFact(body), "unsigned own write", sig, sig)
	require.NoError(t, err)
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)))
	_, serr := verifyCommitSignature(mustCommit(t, svc, h))
	require.Error(t, serr, "setup: the write is unsigned")
	return h
}

func ocSnapshot(t *testing.T, svc *Service) originSnapshot {
	t.Helper()
	s, err := svc.rh.readOriginSnapshot()
	require.NoError(t, err)
	return s
}

func ocTrySync(svc *Service) (SyncResult, error) {
	return svc.Remote().(*remoteIndex).Sync(context.Background(), "agent/test", nil)
}

// ocAcross builds the standard cross-origin fixture and returns the store,
// A, B, the consumed own f and A's foreign x. When diverged, B also wrote on
// its own main.
func ocAcross(t *testing.T, diverged bool) (svc *Service, a, b string, f, x plumbing.Hash) {
	t.Helper()
	svc, a, _ = wmFixture(t)
	b = ocCloneBare(t, a)
	f = ocConsumeOwn(t, svc, a)
	x = ocForeignOnBareMain(t, a, "kb/foreign.md", "someone else's write on A's main")
	wmSync(t, svc)
	require.True(t, reachableFrom(t, svc, "agent/test", x), "setup: the branch took x")
	require.Equal(t, originSnapshot{Identity: originIdentity(fileuri.New(a)), Commit: x}, ocSnapshot(t, svc), "setup: the snapshot is A's main")
	if diverged {
		ocForeignOnBareMain(t, b, "kb/b-own.md", "B's own main")
	}
	return svc, a, b, f, x
}

// ── class 3 and class 1 ──────────────────────────────────────────────────

// After the URL changes A -> B, what A held and B lacks (f, x) is class 3:
// not replayed, not pushed, kept at the left-out ref and reported. This
// copy's unsynced w is class 1: replayed and pushed. The snapshot becomes
// B's main.
// SABOTAGE: reconcile across origins by the existing rules (ignore the
// snapshot identity) -> no LeftOut report / nothing kept -> red.
func TestSync_OriginChangeLeavesTheOldOriginsCommitsOutAndPushesOwnWork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		diverged bool
		detach   bool
	}{
		{"new origin diverged", true, false},
		{"new origin behind", false, false},
		{"detached then attached", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, a, b, f, x := ocAcross(t, tc.diverged)
			writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "own, unsynced")
			if tc.detach {
				require.NoError(t, svc.Remote().DeleteRemote("origin"))
				svc.SetOrigin(nil)
			}

			ocRepoint(t, svc, fileuri.New(b))
			res := wmSync(t, svc)
			require.Equal(t, 2, res.Agent.LeftOut, "f and x are class 3")
			require.Equal(t, originIdentity(fileuri.New(a)), res.Agent.LeftOutOrigin)
			kept, err := svc.rh.gits.Reference(plumbing.ReferenceName(res.Agent.LeftOutRef))
			require.NoError(t, err, "the left-out tip is kept")
			require.True(t, mustAncestor(t, svc, x, kept.Hash()), "the kept tip holds x")
			require.True(t, mustAncestor(t, svc, f, kept.Hash()), "and f")
			require.False(t, reachableFrom(t, svc, "agent/test", x), "x is not on the branch")
			require.False(t, reachableFrom(t, svc, "agent/test", f), "f is not on the branch")
			require.True(t, hasFileAt(t, svc, "agent/test", "kb/w.md"), "w is kept")

			wmPush(t, svc)
			require.True(t, ocBareHasFile(t, b, "agent/test", "kb/w.md"), "w reaches B")
			require.False(t, ocBareHasFile(t, b, "agent/test", "kb/foreign.md"), "x is not pushed")
			require.False(t, ocBareHasFile(t, b, "agent/test", "kb/own.md"), "f is not pushed")

			bMain := mustHeadHash(t, svc, "main")
			require.Equal(t, originSnapshot{Identity: originIdentity(fileuri.New(b)), Commit: bMain}, ocSnapshot(t, svc))
			res2 := wmSync(t, svc)
			require.Zero(t, res2.Agent.LeftOut, "the next round with B has nothing to leave out")
		})
	}
}

// An UNRELATED new origin (no shared history with A): everything A held is
// class 3 and the branch must not stay as it stands (the disjoint no-op of
// the existing rules would push A's whole history to B). With no own
// unsynced work the walk is empty, and the branch is rebuilt on B's main.
// SABOTAGE: an empty walk keeps the branch (drop replayOpts.rebuild) -> A's
// history reaches B -> red.
func TestSync_OriginChangeToAnUnrelatedOriginPushesNothingOfTheOldOne(t *testing.T) {
	svc, _, _, f, x := ocAcross(t, false)
	b := ocUnrelatedBare(t)

	ocRepoint(t, svc, fileuri.New(b))
	res := wmSync(t, svc)
	shared, err := mustCommit(t, svc, x).MergeBase(mustCommit(t, svc, mustHeadHash(t, svc, "main")))
	require.NoError(t, err)
	require.Empty(t, shared, "setup: B shares no history with what this copy had")
	require.Positive(t, res.Agent.LeftOut)
	require.False(t, reachableFrom(t, svc, "agent/test", x))
	require.False(t, reachableFrom(t, svc, "agent/test", f))
	require.Equal(t, mustHeadHash(t, svc, "main"), mustHeadHash(t, svc, "agent/test"), "rebuilt on B's main")
	wmPush(t, svc)
	require.False(t, ocBareHasFile(t, b, "agent/test", "kb/foreign.md"))
	require.False(t, ocBareHasFile(t, b, "agent/test", "kb/own.md"))
}

// ocUnrelatedBare is a bare remote whose main is a root commit of its own
// (content no other fixture writes).
func ocUnrelatedBare(t *testing.T) string {
	t.Helper()
	bare := t.TempDir()
	mustRun(t, "", "git", "init", "-q", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	mustRun(t, "", "git", "init", "-q", "--initial-branch=main", work)
	mustRun(t, work, "git", "config", "user.email", "u@u")
	mustRun(t, work, "git", "config", "user.name", "unrelated")
	require.NoError(t, os.WriteFile(filepath.Join(work, "unrelated.txt"), []byte("an unrelated history"), 0o644))
	mustRun(t, work, "git", "add", "unrelated.txt")
	mustRun(t, work, "git", "commit", "-q", "-m", "unrelated root")
	mustRun(t, work, "git", "push", "-q", bare, "main")
	return bare
}

// mustAncestor reports whether a is an ancestor of (or equal to) b.
func mustAncestor(t *testing.T, svc *Service, a, b plumbing.Hash) bool {
	t.Helper()
	if a == b {
		return true
	}
	ac := mustCommit(t, svc, a)
	ok, err := ac.IsAncestor(mustCommit(t, svc, b))
	require.NoError(t, err)
	return ok
}

// The fleet shape (dead-master fallback, #544 review B1): alpha, the master,
// wrote M (signed by ITS key) on main after bravo last synced from it; bravo
// hosts with confirm_loss and writes on its own main; the member, whose
// branch fast-forwarded onto M, follows bravo. M is class 3: not pushed into
// the member's branch on bravo. The member's unsynced w is pushed.
// SABOTAGE: as above -> M reaches bravo -> red.
func TestSync_FleetMemberAfterDeadMasterFallbackDoesNotPushTheOldMastersWrites(t *testing.T) {
	member, alpha, _ := wmFixture(t)
	bravo := ocCloneBare(t, alpha)
	ocConsumeOwn(t, member, alpha)

	master, err := Open(filepath.Join(t.TempDir(), "alpha.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = master.Close() })
	alphaKey := ocKey(t)
	master.SetSigner(alphaKey)
	_, _, err = master.InitFromRemote(context.Background(), fileuri.New(alpha), nil, "main", "agent/alpha", nil, nil)
	require.NoError(t, err)
	m := plumbing.NewHash(writeMergeFact(t, master, "agent/alpha", "kb/members/delta.md", "Delta", "the master's member record"))
	s, err := verifyCommitSignature(mustCommit(t, master, m))
	require.NoError(t, err)
	fp, err := keyFingerprint(alphaKey.PublicKey())
	require.NoError(t, err)
	require.Equal(t, fp, s.Fingerprint, "setup: M is signed by alpha's key")
	_, err = master.Remote().(*remoteIndex).Push(context.Background(), "agent/alpha", nil)
	require.NoError(t, err)
	wmSetOriginMain(t, alpha, m)

	wmSync(t, member)
	require.True(t, reachableFrom(t, member, "agent/test", m), "setup: the member's branch fast-forwarded onto M")
	writeMergeFact(t, member, "agent/test", "kb/w.md", "W", "the member's own, unsynced")
	ocForeignOnBareMain(t, bravo, "kb/host.md", "bravo hosts (confirm_loss)")

	ocRepoint(t, member, fileuri.New(bravo))
	res := wmSync(t, member)
	require.Positive(t, res.Agent.LeftOut)
	require.False(t, hasFileAt(t, member, "agent/test", "kb/members/delta.md"), "M is not on the branch")
	require.True(t, hasFileAt(t, member, "agent/test", "kb/w.md"))
	wmPush(t, member)
	require.False(t, ocBareHasFile(t, bravo, "agent/test", "kb/members/delta.md"), "M does not reach bravo")
	require.True(t, ocBareHasFile(t, bravo, "agent/test", "kb/w.md"))
}

// Unsynced own work is class 1 whoever signed it (#544 re-review R1): a write
// signed by a key the instance no longer has, and an unsigned write, are not
// reachable from the snapshot, so they are replayed and pushed.
// SABOTAGE: hold every commit not signed by the current key -> red.
func TestSync_OriginChangeKeepsUnsyncedOwnWorkWhoeverSignedIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, svc *Service)
	}{
		{"signed by an old instance key", func(t *testing.T, svc *Service) {
			svc.SetSigner(ocKey(t))
			writeMergeFact(t, svc, "agent/test", "kb/mine.md", "Mine", "own, unsynced")
			svc.SetSigner(ocKey(t))
		}},
		{"unsigned", func(t *testing.T, svc *Service) {
			ocUnsignedWrite(t, svc, "agent/test", "kb/mine.md", "Mine")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, b, _, _ := ocAcross(t, true)
			tc.write(t, svc)
			ocRepoint(t, svc, fileuri.New(b))
			wmSync(t, svc)
			require.True(t, hasFileAt(t, svc, "agent/test", "kb/mine.md"), "the unsynced own write is kept")
			require.False(t, hasFileAt(t, svc, "agent/test", "kb/foreign.md"))
			wmPush(t, svc)
			require.True(t, ocBareHasFile(t, b, "agent/test", "kb/mine.md"), "it reaches B")
			require.False(t, ocBareHasFile(t, b, "agent/test", "kb/foreign.md"))
		})
	}
}

// ── failure at every step converges ─────────────────────────────────────

// A round with B that fails at ANY step leaves the snapshot as A's, and the
// next round computes the same classification: the end state is the same as
// a round that never failed (#544 re-review R2 is the "after the main
// rewind" case: round 2's main reconcile is a no-op, the agent is still
// classified against the snapshot).
// SABOTAGE: write the snapshot before the reconcile -> red in the
// reconcile-failure subtests; route by main's mode -> red in "after the main
// rewind".
func TestSync_OriginChangeConvergesAfterAFailureAtAnyStep(t *testing.T) {
	failed := errors.New("seam: this step fails")
	for _, tc := range []struct {
		name  string
		hooks func(once *bool) storeHooks
		fails string // "sync" or "push"
	}{
		{"fetch", func(once *bool) storeHooks {
			return storeHooks{afterFetch: func() error { return failOnce(once, failed) }}
		}, "sync"},
		{"after the main rewind", func(once *bool) storeHooks {
			return storeHooks{afterMainReconcile: func(string) error { return failOnce(once, failed) }}
		}, "sync"},
		{"snapshot write", func(once *bool) storeHooks {
			return storeHooks{beforeOriginSnapshot: func() error { return failOnce(once, failed) }}
		}, "sync"},
		{"push", func(once *bool) storeHooks {
			return storeHooks{beforePush: func(string) error { return failOnce(once, failed) }}
		}, "push"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, a, b, f, x := ocAcross(t, true)
			writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "own, unsynced")
			once := false
			setStoreHooks(t, tc.hooks(&once))

			ocRepoint(t, svc, fileuri.New(b))
			// Round 1 fails at the step.
			_, serr := ocTrySync(svc)
			if tc.fails == "sync" {
				require.ErrorIs(t, serr, failed)
				require.Equal(t, originIdentity(fileuri.New(a)), ocSnapshot(t, svc).Identity, "a failed round keeps A's snapshot")
			} else {
				require.NoError(t, serr)
				_, perr := svc.Remote().(*remoteIndex).Push(context.Background(), "agent/test", nil)
				require.ErrorIs(t, perr, failed)
			}
			// Round 2 runs whole.
			wmSync(t, svc)
			wmPush(t, svc)

			require.False(t, reachableFrom(t, svc, "agent/test", x))
			require.False(t, reachableFrom(t, svc, "agent/test", f))
			require.True(t, hasFileAt(t, svc, "agent/test", "kb/w.md"))
			require.True(t, ocBareHasFile(t, b, "agent/test", "kb/w.md"), "w reaches B")
			require.False(t, ocBareHasFile(t, b, "agent/test", "kb/foreign.md"), "x never reaches B")
			require.False(t, ocBareHasFile(t, b, "agent/test", "kb/own.md"), "f never reaches B")
			require.Equal(t, originSnapshot{Identity: originIdentity(fileuri.New(b)), Commit: mustHeadHash(t, svc, "main")}, ocSnapshot(t, svc))
			require.True(t, leftOutKept(t, svc, x), "the left-out tip holding x is kept")
		})
	}
}

func failOnce(once *bool, err error) error {
	if *once {
		return nil
	}
	*once = true
	return err
}

// leftOutKept reports whether some left-out ref holds h.
func leftOutKept(t *testing.T, svc *Service, h plumbing.Hash) bool {
	t.Helper()
	iter, err := svc.rh.gits.IterReferences()
	require.NoError(t, err)
	found := false
	require.NoError(t, iter.ForEach(func(r *plumbing.Reference) error {
		if strings.HasPrefix(r.Name().String(), leftOutRefPrefix) && mustAncestor(t, svc, h, r.Hash()) {
			found = true
		}
		return nil
	}))
	return found
}

// ── same origin, credentials, spellings, push mirror ─────────────────────

// A commit the SAME origin removed is not resurrected (class 2, the
// existing rules), and nothing is reported as left out.
// SABOTAGE: treat every snapshot as another origin's -> LeftOut reported -> red.
func TestSync_SameOriginRemovedCommitIsNotResurrected(t *testing.T) {
	svc, a, r := wmFixture(t)
	f := ocConsumeOwn(t, svc, a)
	wmSetOriginMain(t, a, r)
	res := wmSync(t, svc)
	require.Equal(t, ModeRewound, res.Main.Mode)
	require.Zero(t, res.Agent.LeftOut, "the same origin: class 2, nothing surfaced")
	require.False(t, reachableFrom(t, svc, "agent/test", f))
	wmPush(t, svc)
	require.False(t, ocBareHasFile(t, a, "agent/test", "kb/own.md"), "not pushed back")
}

// A credential change (in the origin record, or inside the URL) and another
// spelling of the same origin (a dot segment, .git, a trailing slash) keep
// the identity: a later scrub on that origin is class 2, nothing is
// surfaced.
// SABOTAGE: compare the raw URL -> LeftOut reported -> red.
func TestSync_SameOriginAnotherSpellingOrCredentialIsNotAChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  func(a string) (string, *Origin)
	}{
		{"credential in the record", func(a string) (string, *Origin) {
			return fileuri.New(a), &Origin{URL: fileuri.New(a), Branch: "main", AuthMethod: "basic", AuthToken: "new-secret"}
		}},
		{"credential in the URL", func(a string) (string, *Origin) {
			return fileuri.New(a), &Origin{URL: "file://user:token@" + strings.TrimPrefix(fileuri.New(a), "file://") + "/", Branch: "main"}
		}},
		{"another spelling", func(a string) (string, *Origin) {
			u := fileuri.New(a)
			i := strings.LastIndex(u, "/")
			return u, &Origin{URL: u[:i] + "/." + u[i:] + ".git/", Branch: "main"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, a, r := wmFixture(t)
			f := ocConsumeOwn(t, svc, a)
			gitURL, o := tc.url(a)
			require.NoError(t, svc.ConfigureRemote(gitURL, "main", "agent/test"))
			svc.SetOrigin(o)
			wmSetOriginMain(t, a, r)
			res := wmSync(t, svc)
			require.Zero(t, res.Agent.LeftOut)
			require.False(t, reachableFrom(t, svc, "agent/test", f), "f stays removed")
		})
	}
}

// B already holds f in main but has no agent branch: the stale tracking ref
// for the agent branch, left from A, used to make Push report "nothing to
// push". A fetch from an origin without the branch now drops that ref.
// SABOTAGE: keep the tracking ref when the origin lacks the branch -> red.
func TestSync_OriginChangePushesTheBranchTheNewOriginLacks(t *testing.T) {
	svc, a, _ := wmFixture(t)
	ocConsumeOwn(t, svc, a)
	b := ocCloneBare(t, a)
	mustRun(t, b, "git", "update-ref", "-d", "refs/heads/agent/test")

	ocRepoint(t, svc, fileuri.New(b))
	res := wmSync(t, svc)
	require.Zero(t, res.Agent.LeftOut, "B holds everything A had")
	_, err := svc.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", "agent/test"))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "the tracking ref mirrors B, which has no agent branch")
	pres, err := svc.Remote().(*remoteIndex).Push(context.Background(), "agent/test", nil)
	require.NoError(t, err)
	require.True(t, pres.Pushed)
	require.True(t, ocBareHasFile(t, b, "agent/test", "kb/own.md"))
}

// With no snapshot (a repository last synced before this existed) there is
// nothing to classify against: the existing rules apply, and the first round
// that succeeds writes the snapshot.
// SABOTAGE: never write the snapshot -> red.
func TestSync_FirstSuccessfulRoundWritesTheSnapshot(t *testing.T) {
	svc, a, r := wmFixture(t)
	_, err := svc.rh.db.Exec(`DELETE FROM meta WHERE key = ?`, originSnapshotKey)
	require.NoError(t, err)
	require.False(t, ocSnapshot(t, svc).ok())
	wmSync(t, svc)
	require.Equal(t, originSnapshot{Identity: originIdentity(fileuri.New(a)), Commit: r}, ocSnapshot(t, svc))
}

// ── identity ─────────────────────────────────────────────────────────────

// SABOTAGE: compare the raw URL / no path cleaning / scp not mapped /
// credentials kept in an unparseable URL -> red.
func TestOriginIdentity(t *testing.T) {
	same := [][2]string{
		{"https://forge.example/team/kb.git", "https://user:new-token@forge.example/team/kb.git"},
		{"https://forge.example/team/kb.git", "HTTPS://Forge.Example:443/team/kb.git/"},
		{"http://forge.example/kb", "http://forge.example:80/kb"},
		{"/srv/kb.git", "/srv/kb.git/"},
		{"https://forge.example/team/kb", "https://forge.example/team/kb.git"},
		{"https://forge.example/team/kb", "https://forge.example/team/./kb"},
		{"https://forge.example/team/kb", "https://forge.example/team/x/../kb/"},
		{"https://forge.example/team/kb", "https://forge.example//team/kb"},
		{"git@forge.example:team/kb.git", "ssh://git@forge.example/team/kb"},
		{"git@forge.example:team/kb", "ssh://forge.example:22/team/kb.git"},
		{"/srv/kb", "file:///srv/kb"},
		{"/srv/./kb", "file:///srv/kb.git"},
	}
	for _, p := range same {
		require.Equal(t, originIdentity(p[0]), originIdentity(p[1]), "%q vs %q", p[0], p[1])
	}
	// Still a change (accepted as known).
	differ := [][2]string{
		{"https://forge.example/team/kb.git", "https://forge.example/team/fork.git"},
		{"https://forge.example/kb", "https://other.example/kb"},
		{"https://forge.example:8443/kb", "https://forge.example/kb"},
		{"https://forge.example/kb", "ssh://forge.example/kb"},
		{"https://forge.example/Team/kb", "https://forge.example/team/kb"},
		{"http://forge.example/kb", "https://forge.example/kb"},
		{"git@forge.example:team/kb", "git@other.example:team/kb"},
		{"/srv/kb", "/srv/kb2"},
	}
	for _, p := range differ {
		require.NotEqual(t, originIdentity(p[0]), originIdentity(p[1]), "%q vs %q", p[0], p[1])
	}
	// No credential survives, parseable or not (#544 review N1, re-review N1).
	for _, raw := range []string{
		"https://user:s3cret@forge.example/kb",
		"https://user:se%zzs3cret@forge.example/kb",
		"https://s3cret@forge.example:99999x/kb",
		"user:s3cret@forge.example:team/kb",
		"https://user:s3cret/x@forge.example/kb",
		"https://user:s3cret#x@forge.example/kb",
		"https://user:s3cret?x@forge.example/kb",
		"https://user:p@s3cret@forge.example/kb",
	} {
		require.NotContains(t, originIdentity(raw), "s3cret", raw)
	}
}
