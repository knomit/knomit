package store

import (
	"context"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// mainWrite commits one file on the push host's main and returns that
// commit's committer time.
func (h *pushHost) mainWrite(p, content string) time.Time {
	h.t.Helper()
	_, _, err := h.svc.Facts().(*factIndex).writeFileExact(context.Background(), "main", p, content, "write "+p, "edit", "")
	require.NoError(h.t, err)
	c, err := object.GetCommit(h.svc.rh.gits, h.ref("main"))
	require.NoError(h.t, err)
	return c.Committer.When.UTC()
}

// FleetSince is, per path, the committer time of the first-parent commit
// that brought the path's CURRENT blob to main: a later edit moves it, a
// later commit that leaves it alone does not, a path the tip lacks has none,
// and a version older than the commits walked has none.
// SABOTAGE: report the time of the first commit visited (the tip) for every
// path -> boss and x are the README's time -> red; ignore maxCommits -> x is
// found within 2 -> red.
func TestFleetSince_TheCommitThatBroughtTheCurrentVersion(t *testing.T) {
	h := newFleetPushHost(t, nil)
	const boss, x = "kb/members/boss/1.md", "kb/members/x/1.md"
	tx := h.mainWrite(x, memberFile("x", fact.MemberActive, namedSigner(t, "x").PublicKey()))
	time.Sleep(1100 * time.Millisecond) // commit times have whole seconds
	tHost := h.mainWrite(fact.FleetHostPath, "# knomit fleet host v1\nurl: https://h.example/git/fleet\n")
	time.Sleep(1100 * time.Millisecond)
	tBoss := h.mainWrite(boss, memberFile("boss", fact.MemberActive, namedSigner(t, "boss").PublicKey())+"\nv2\n")
	time.Sleep(1100 * time.Millisecond)
	tReadme := h.mainWrite("README.md", "x")
	require.True(t, tx.Before(tHost) && tHost.Before(tBoss) && tBoss.Before(tReadme), "fixture: four distinct commit times")

	got, err := h.svc.FleetSince("main", []string{boss, x, fact.FleetHostPath, "kb/members/nobody/1.md"}, 100)
	require.NoError(t, err)
	require.Equal(t, map[string]time.Time{boss: tBoss, x: tx, fact.FleetHostPath: tHost}, got)

	got, err = h.svc.FleetSince("main", []string{boss, x}, 2)
	require.NoError(t, err)
	require.Equal(t, map[string]time.Time{boss: tBoss}, got, "x's version is older than the 2 commits walked")
}
