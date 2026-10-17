package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/platform/fileuri"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// #544 review B1. After the old origin's position is forgotten, the replay
// walk runs down to the first commit the new origin holds, and what it passes
// includes what the OLD origin wrote (this branch fast-forwarded onto its
// main). Replayed, those commits are re-signed as this copy and pushed to the
// new origin; in a fleet, a member pushing the old master's writes is refused
// by the new host for good. So the first sync after a forget replays only
// commits this instance signed (user decision 2026-10-17, option (a)).

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

// ocBareBranchHas reports whether the bare remote's branch holds commit h.
func ocBareBranchHas(t *testing.T, bare, branch string, h plumbing.Hash) bool {
	t.Helper()
	c := exec.Command("git", "merge-base", "--is-ancestor", h.String(), "refs/heads/"+branch)
	c.Dir = bare
	return c.Run() == nil
}

// A foreign commit on the old origin's main (a squash merge, another
// writer's direct commit) that the new origin lacks is not replayed and not
// pushed; this copy's own work is. Two shapes: the new origin diverged (it
// wrote on its own main: the rebase walk) and the new origin is behind (the
// branch is strictly ahead of it: kept as it stands only when nothing foreign
// lies above it).
// SABOTAGE: own-only mode off (reconcileAgentRebase passes false) -> x is
// replayed and pushed to B -> red in both subtests.
func TestSync_OriginChangeDoesNotReplayTheOldOriginsForeignCommit(t *testing.T) {
	for _, diverged := range []bool{true, false} {
		name := "new origin behind"
		if diverged {
			name = "new origin diverged"
		}
		t.Run(name, func(t *testing.T) {
			svc, a, _ := wmFixture(t)
			b := ocCloneBare(t, a) // B: main r
			ocConsumeOwn(t, svc, a)
			x := ocForeignOnBareMain(t, a, "kb/foreign.md", "someone else's write on A's main")
			wmSync(t, svc)
			require.True(t, reachableFrom(t, svc, "agent/test", x), "setup: the branch took x")
			writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "own, unsynced")
			if diverged {
				ocForeignOnBareMain(t, b, "kb/b-own.md", "B's own main")
			}

			ocRepoint(t, svc, fileuri.New(b))
			wmSync(t, svc)
			require.False(t, hasFileAt(t, svc, "agent/test", "kb/foreign.md"), "x is not replayed")
			require.True(t, hasFileAt(t, svc, "agent/test", "kb/own.md"), "own consumed f is kept")
			require.True(t, hasFileAt(t, svc, "agent/test", "kb/w.md"), "own unsynced w is kept")
			forgotten, err := svc.rh.originPositionForgotten()
			require.NoError(t, err)
			require.False(t, forgotten, "the reconcile succeeded, so the own-only mode ends")

			wmPush(t, svc)
			require.False(t, ocBareHasFile(t, b, "agent/test", "kb/foreign.md"), "x does not reach B")
			require.False(t, ocBareBranchHas(t, b, "agent/test", x))
			require.True(t, ocBareHasFile(t, b, "agent/test", "kb/own.md"), "f reaches B")
			require.True(t, ocBareHasFile(t, b, "agent/test", "kb/w.md"), "w reaches B")
		})
	}
}

// The fleet shape (dead-master fallback, #544 review B1): alpha, the master,
// wrote M (signed by ITS key) on the fleet's main after bravo last synced
// from it; alpha dies, bravo hosts with confirm_loss (and writes on its own
// main), and this member follows bravo. The member's branch had fast-
// forwarded onto alpha's M. It must not replay M, re-signed as itself, into
// its branch on bravo (bravo's judge would refuse that for good); its own
// work, including what alpha consumed but bravo lacks, is kept and pushed.
// SABOTAGE: own-only mode off -> M replayed and on bravo -> red.
func TestSync_FleetMemberAfterDeadMasterFallbackDoesNotPushTheOldMastersWrites(t *testing.T) {
	member, alpha, _ := wmFixture(t)
	bravo := ocCloneBare(t, alpha) // bravo's last sync from alpha: main r
	ocConsumeOwn(t, member, alpha)

	// Alpha, with its own key, writes M and publishes it on main.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	alphaKey, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	master, err := Open(filepath.Join(t.TempDir(), "alpha.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = master.Close() })
	master.SetSigner(alphaKey)
	_, _, err = master.InitFromRemote(context.Background(), fileuri.New(alpha), nil, "main", "agent/alpha", nil, nil)
	require.NoError(t, err)
	m := plumbing.NewHash(writeMergeFact(t, master, "agent/alpha", "kb/members/delta.md", "Delta", "the master's member record"))
	s, err := verifyCommitSignature(mustCommit(t, master, m))
	require.NoError(t, err, "setup: M is signed")
	alphaFP, err := keyFingerprint(alphaKey.PublicKey())
	require.NoError(t, err)
	require.Equal(t, alphaFP, s.Fingerprint, "setup: M is signed by alpha's key")
	_, err = master.Remote().(*remoteIndex).Push(context.Background(), "agent/alpha", nil)
	require.NoError(t, err)
	wmSetOriginMain(t, alpha, m)

	wmSync(t, member)
	require.True(t, reachableFrom(t, member, "agent/test", m), "setup: the member's branch fast-forwarded onto M")
	writeMergeFact(t, member, "agent/test", "kb/w.md", "W", "the member's own, unsynced")
	ocForeignOnBareMain(t, bravo, "kb/host.md", "bravo hosts (confirm_loss)")

	ocRepoint(t, member, fileuri.New(bravo))
	wmSync(t, member)
	require.False(t, hasFileAt(t, member, "agent/test", "kb/members/delta.md"), "M is not replayed")
	require.True(t, hasFileAt(t, member, "agent/test", "kb/own.md"), "own f, consumed by alpha, is kept")
	require.True(t, hasFileAt(t, member, "agent/test", "kb/w.md"))

	wmPush(t, member)
	require.False(t, ocBareHasFile(t, bravo, "agent/test", "kb/members/delta.md"), "M does not reach bravo")
	require.False(t, ocBareBranchHas(t, bravo, "agent/test", m))
	require.True(t, ocBareHasFile(t, bravo, "agent/test", "kb/own.md"))
	require.True(t, ocBareHasFile(t, bravo, "agent/test", "kb/w.md"))
}

// #544 review S1: the new tag is recorded only after a successful fetch. A
// round that stops before the fetch (no git remote yet: the early return
// comes BEFORE the tracking refs are forgotten) or whose fetch fails leaves
// the old tag, so the next round forgets again. Recorded early, the next
// round would not forget, would seed the watermark from A's tracking ref and
// drop f.
// SABOTAGE: record the tag before the fetch -> red (the tag assert in both
// subtests; f dropped in "no git remote yet").
func TestSync_OriginTagIsRecordedOnlyAfterASuccessfulFetch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first func(t *testing.T, svc *Service, b string)
	}{
		{"no git remote yet", func(t *testing.T, svc *Service, b string) {
			require.NoError(t, svc.Remote().DeleteRemote("origin"))
			svc.SetOrigin(&Origin{URL: fileuri.New(b), Branch: "main"})
			wmSync(t, svc) // skips: no git remote
		}},
		{"the fetch fails", func(t *testing.T, svc *Service, b string) {
			missing := filepath.Join(t.TempDir(), "missing.git")
			require.NoError(t, svc.ConfigureRemote(fileuri.New(missing), "main", "agent/test"))
			svc.SetOrigin(&Origin{URL: fileuri.New(b), Branch: "main"})
			_, err := svc.Remote().(*remoteIndex).Sync(context.Background(), "agent/test", nil)
			require.Error(t, err, "setup: the fetch fails")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, a, _ := wmFixture(t)
			b := ocCloneBare(t, a)
			f := ocConsumeOwn(t, svc, a)

			tc.first(t, svc, b)
			tag, err := svc.rh.readOriginPositionURL()
			require.NoError(t, err)
			require.Equal(t, originIdentity(fileuri.New(a)), tag, "no successful fetch from B: the tag is still A's")

			ocRepoint(t, svc, fileuri.New(b))
			wmSync(t, svc)
			require.True(t, reachableFrom(t, svc, "agent/test", f), "f is kept")
			wmPush(t, svc)
			require.True(t, ocBareHasFile(t, b, "agent/test", "kb/own.md"), "f reaches B")
		})
	}
}

// #544 review S2: the same origin in another spelling (a dot segment, a
// .git suffix, a trailing slash) is not a change, so a commit that origin
// removed is not resurrected and pushed back to it.
// SABOTAGE: compare the raw URL -> f resurrected -> red.
func TestSync_SameOriginInAnotherSpellingIsNotAChange(t *testing.T) {
	svc, a, r := wmFixture(t)
	f := ocConsumeOwn(t, svc, a)
	wmSetOriginMain(t, a, r) // the origin removes f

	u := fileuri.New(a)
	i := strings.LastIndex(u, "/")
	spelled := u[:i] + "/." + u[i:] + ".git/"
	require.NotEqual(t, u, spelled)
	svc.SetOrigin(&Origin{URL: spelled, Branch: "main"})
	wmSync(t, svc)
	require.False(t, reachableFrom(t, svc, "agent/test", f), "f stays removed")
	wmPush(t, svc)
	require.False(t, ocBareHasFile(t, a, "agent/test", "kb/own.md"), "f is not pushed back")
}
