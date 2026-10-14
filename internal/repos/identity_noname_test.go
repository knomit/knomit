package repos

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/pki"
)

// D18/P1b: the instance's {agent} and {host}, its member-record id and the
// commit author all come from the PERSISTED identity (Deps.AgentID /
// Deps.AgentHost, read from instance_identity), never from the agent
// branch's name. The fixture's hostname ends in "-<8 hex>" and the branch is
// agent/<that hostname>, so the old derivation would have produced
// {agent} = "ci-0badcafe" and {host} = "ci" (the "fp8" trimmed off).
// Sabotage: build the dispatcher's identity from agentBranch again (strip
// "agent/", trim a trailing -<8 hex>) — red on both fields.
func TestTriggerIdentity_ComesFromThePersistedIdentity(t *testing.T) {
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb"},
		AgentBranch: "agent/ci-0badcafe",
		AgentID:     "opaque-1",
		AgentHost:   "ci-0badcafe",
		KeyPath:     filepath.Join(home, "agent.key"),
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	t.Cleanup(func() { m.Close() })

	ri := m.newInstance("r", "uid-r", false)
	require.NotNil(t, ri.triggers, "fixture: a writable repo with an agent branch has a dispatcher")
	require.Equal(t, fact.TriggerIdentity{Agent: "opaque-1", Host: "ci-0badcafe"}, ri.triggers.identity)
	require.Equal(t, "opaque-1", m.AgentID(), "the fleet code's own id is the persisted one")
	require.Equal(t, "opaque-1", m.advertised().Agent, "the member record names the persisted id")
	require.Equal(t, "opaque-1", ri.env.agentID, "the store the stages open authors as the persisted id")
}

// End to end through the Manager: a repo's store is opened by the stages
// with Deps.AgentID, so a fact written on the agent branch is authored as the
// persisted id even though the branch's name would derive another
// ("agent/test" -> "test", the fixture fallback). Sabotage: drop
// svc.SetAgentID(env.agentID) in stages.go — red: author "test".
func TestManagerStore_AuthorsAsThePersistedAgentID(t *testing.T) {
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb"},
		AgentBranch: "agent/test",
		AgentID:     "opaque-2",
		KeyPath:     filepath.Join(home, "agent.key"),
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	t.Cleanup(func() { m.Close() })
	ri := bootNamedRepo(t, m, "r")
	svc := testService(t, ri)
	ctx := context.Background()
	_, err := svc.Facts().WriteFact(ctx, ri.AgentBranch(), "kb/notes/a.md",
		"---\ntype: observation\nconfidence: 0.9\n---\n# a\n\nbody\n", "learn: a", "learn")
	require.NoError(t, err)
	tip, err := svc.Branches().HeadCommit(ctx, ri.AgentBranch())
	require.NoError(t, err)
	info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(tip))
	require.NoError(t, err)
	require.Equal(t, "opaque-2", info.AuthorName)
	require.Equal(t, "opaque-2+learn@agents.knomit.io", info.AuthorEmail)
}

// The identity is written ONCE with its host label and read back as stored:
// a later boot with a different fresh identity (another hostname, another
// key) gets the stored one, host included. An identity row from before the
// host was stored reads back with an empty host — nothing recovers one by
// parsing the branch.
func TestResolveIdentity_PersistsTheHostLabel(t *testing.T) {
	reg, err := OpenRegistry(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })
	db := reg.DB()

	first := Identity{AgentID: "ci-0badcafe-12345678", Branch: "agent/ci-0badcafe-12345678", Host: "ci-0badcafe"}
	got, err := resolveIdentity(db, first, fixedNow())
	require.NoError(t, err)
	require.Equal(t, first, got)

	got, err = resolveIdentity(db, Identity{AgentID: "other-87654321", Branch: "agent/other-87654321", Host: "other"}, fixedNow())
	require.NoError(t, err)
	require.Equal(t, first, got, "the stored identity wins, host label included")

	_, err = db.Exec(`DELETE FROM instance_identity_host`)
	require.NoError(t, err)
	got, err = resolveIdentity(db, Identity{AgentID: "x", Branch: "agent/x", Host: "x"}, fixedNow())
	require.NoError(t, err)
	require.Equal(t, Identity{AgentID: first.AgentID, Branch: first.Branch}, got, "no stored host: empty, not parsed from the branch")

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM instance_identity_host`).Scan(&n))
	require.Equal(t, 0, n, "reading never backfills a host")
}

// R2: an agent id is minted only if it reads back as itself from the address
// it commits as and its certificate carries, <id>@agents.knomit.io. An id
// holding '+' (read back as the part before it) or '@' is refused and nothing
// is written; a plain id is then minted normally.
func TestResolveIdentity_RefusesToMintAnIDThatDoesNotReadBack(t *testing.T) {
	reg, err := OpenRegistry(filepath.Join(t.TempDir(), "control.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })
	db := reg.DB()

	for _, id := range []string{"lap+top-12345678", "lap@top-12345678"} {
		_, err := resolveIdentity(db, Identity{AgentID: id, Branch: "agent/" + id, Host: "h"}, fixedNow())
		require.ErrorIs(t, err, pki.ErrAgentIDInvalid, id)
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM instance_identity`).Scan(&n))
		require.Equal(t, 0, n, "%s: nothing was minted", id)
	}
	ok := Identity{AgentID: "laptop-12345678", Branch: "agent/laptop-12345678", Host: "laptop"}
	got, err := resolveIdentity(db, ok, fixedNow())
	require.NoError(t, err)
	require.Equal(t, ok, got)
}
