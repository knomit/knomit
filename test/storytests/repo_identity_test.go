// Regression test for the repo registry's identity-uniqueness constraint:
// one local copy per knowledge base. Two local repos backed by the same
// knowledge base would both write agent/<host> and clobber each other on
// push to a shared origin, so Registry.RecordRepoID rejects a second ACTIVE
// repo whose root commit matches one already registered, and Manager.Create
// rolls the failed clone back entirely.
package storytests

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/test/testenv"
)

// TestCreate_MirrorCloneRejected is the regression test for the whole
// constraint: it exercises the ONE case a plain origin-URL comparison
// (Manager.ActiveRepoWithOrigin) cannot see. Clone origin A as "alpha", then
// clone a MIRROR of A — a different URL serving byte-identical history,
// therefore the same root commit — as "beta". The URL preflight passes (the
// URLs differ); only the post-clone identity check (Registry.RecordRepoID)
// catches it, and Create must unwind the second clone completely: no live
// repo, no registry row, no leftover .db file.
//
// This test lives in test/storytests rather than internal/repos for two
// reasons:
//
//  1. Import cycle: the mirror fixture lives in test/testenv (per the task
//     brief, alongside the existing BareRemote/BareRemoteHTTP fixtures), and
//     test/testenv already imports knomit/internal/repos (see
//     mcp_hypothesize.go, storyboard.go) to drive Manager.Create in its own
//     Storyboard/RepoHandle DSL. internal/repos importing test/testenv back
//     would be a genuine import cycle, so a test that needs the testenv
//     fixture cannot live in package repos.
//  2. Storyboard.Repo cannot be reused for either side of this test even
//     though it already knows how to Connect a repo to a remote: it boots a
//     SEPARATE repos.Manager (and therefore a separate control.db) per repo
//     name specifically to isolate repos from one another. That isolation is
//     exactly what this test must NOT have — alpha and beta must share one
//     registry, or the uniqueness constraint being tested is never exercised
//     at all. So this test builds its own repos.Manager directly (mirroring
//     the internal/repos test package's own newTestManager helper) and uses
//     the Storyboard only for its bare-remote and mirror fixtures.
//
// Because this test cannot reach the unexported Manager.reg field the
// brief's original draft used, "the registry has exactly one active row" is
// asserted by reopening the same control.db through the exported
// repos.OpenRegistry/Registry.List — the identical on-disk state, read
// through the production accessor instead of a private field.
func TestCreate_MirrorCloneRejected(t *testing.T) {
	sb := testenv.NewStoryboard(t)

	origin := sb.BareRemote("origin")
	origin.WriteMain("kb/seed.md", testenv.Fact("seed"), "seed on origin")
	mirror := sb.MirrorOf("mirror", origin)

	// Fixture sanity: the mirror must genuinely share origin's root commit,
	// not merely similar content. If MirrorOf replayed commits instead of
	// cloning bytes, the hashes would differ and this whole test would prove
	// nothing regardless of which way the assertions below come out.
	require.Equal(t, rootCommitOf(t, origin.Dir(), origin.UpstreamBranch()),
		rootCommitOf(t, mirror.Dir(), mirror.UpstreamBranch()),
		"mirror fixture must share origin's root commit byte-for-byte")

	// One shared home/control.db for both Creates below — the constraint
	// under test is enforced BY that shared registry.
	home := filepath.Join(sb.HomeDir(), "shared-manager")
	m := repos.New(context.Background(), repos.Deps{
		Cfg: config.Config{
			Home:            home,
			OntologyRoot:    "kb",
			LocalOriginRoot: sb.HomeDir(), // covers both origin and mirror, under sb's remotes/ dir
		},
		AgentBranch:           "agent/test",
		KeyPath:               filepath.Join(home, "agent.key"),
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })

	_, err := m.Create(context.Background(), repos.CreateSpec{
		Name: "alpha", Mode: "clone", Origin: &repos.OriginSpec{URL: origin.URL()},
	}, nil)
	require.NoError(t, err)

	_, err = m.Create(context.Background(), repos.CreateSpec{
		Name: "beta", Mode: "clone", Origin: &repos.OriginSpec{URL: mirror.URL()},
	}, nil)
	require.ErrorIs(t, err, repos.ErrRepoAlreadyRegistered)

	// The rejected create leaves nothing: no live repo, no registry row.
	require.Nil(t, m.Get("beta"))

	reg, err := repos.OpenRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer reg.Close()
	active, err := reg.List(repos.StateActive)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "alpha", active[0].Name)

	// ...and no file: a rollback that unregisters the row but leaves the
	// .db behind is the failure mode most likely to be lurking in the
	// rollback path. Filter out each repo's own "<uid>.sessions.db" sidecar
	// (store.SessionDBSuffix, itself created eagerly by store.Open) — it also
	// matches "*.db" and would otherwise double-count alpha's own files as a
	// false leftover.
	allDBFiles, err := filepath.Glob(filepath.Join(home, "repos", "*.db"))
	require.NoError(t, err)
	dbFiles := make([]string, 0, len(allDBFiles))
	for _, p := range allDBFiles {
		if store.IsSessionDBFile(filepath.Base(p)) {
			continue
		}
		dbFiles = append(dbFiles, p)
	}
	require.Len(t, dbFiles, 1, "rollback must not leave a leftover .db file: %v (all matches: %v)", dbFiles, allDBFiles)
}

// rootCommitOf returns the hash of dir's single root commit on branch (the
// commit with no parents), failing the test if there is not exactly one.
func rootCommitOf(t *testing.T, dir, branch string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-list", "--max-parents=0", branch).Output()
	require.NoError(t, err, "rev-list root commit in %s", dir)
	lines := strings.Fields(strings.TrimSpace(string(out)))
	require.Len(t, lines, 1, "expected exactly one root commit in %s, got %v", dir, lines)
	return lines[0]
}
