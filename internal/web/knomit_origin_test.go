package web

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// originFactBody is a minimal valid fact: the frontmatter the indexer needs
// and nothing else.
func originFactBody(title string) string {
	return "---\ntype: observation\nconfidence: 0.9\nsources: 1\ndomain: [peering]\n" +
		"entities: []\nrefs: []\n---\n# " + title + "\n\n" + title + "\n"
}

// newPeerManager builds one knomit instance. Background sync is disabled so
// the reconciles this test cares about are the ones it performs itself.
func newPeerManager(t *testing.T, agent string) *repos.Manager {
	t.Helper()
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: home, OntologyRoot: "kb"},
		AgentBranch:           agent,
		KeyPath:               filepath.Join(home, "agent.key"),
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// The goal of the whole change: instance B subscribes to instance A's repo
// through A's /git endpoint, reads A's facts, and sees new ones after A's
// local reconcile publishes them on main.
//
// Every piece this exercises was broken before: A's advertisement pointed HEAD
// at its own agent branch, A's main never moved off the root commit, and the
// probe B runs on the way in came back as an HTTP 500.
func TestKnomitOrigin_SubscribeFollowsPeerMain(t *testing.T) {
	ctx := context.Background()

	a := newPeerManager(t, "agent/a")
	riA, err := a.Create(ctx, repos.CreateSpec{Name: "kb", Mode: "preset"}, nil)
	require.NoError(t, err)
	require.NoError(t, riA.WithRead(func(s *store.Service) {
		_, werr := s.Facts().WriteFact(ctx, "agent/a", "kb/first.md",
			originFactBody("first"), "first", "")
		require.NoError(t, werr)
		res, rerr := s.AdvanceLocalUpstream(ctx, "agent/a", "main")
		require.NoError(t, rerr)
		require.Equal(t, store.ModeFF, res.Mode)
	}))

	srvA := httptest.NewServer(GitRemoteHandler(a))
	defer srvA.Close()
	originURL := srvA.URL + "/kb"

	b := newPeerManager(t, "agent/b")
	riB, err := b.Create(ctx, repos.CreateSpec{
		Name: "peer", Mode: "subscribe", Origin: &repos.OriginSpec{URL: originURL},
	}, nil)
	require.NoError(t, err)
	require.True(t, riB.Subscribed())
	require.Equal(t, "main", riB.ReadBranch(),
		"B must follow A's consensus branch, not the agent branch A's HEAD used to point at")
	require.NoError(t, riB.WithRead(func(s *store.Service) {
		f, rerr := s.Facts().ReadFact(ctx, "main", "kb/first.md", nil)
		require.NoError(t, rerr)
		require.Contains(t, f.Content, "first")
	}))

	// A learns more; its local reconcile publishes it on main.
	require.NoError(t, riA.WithRead(func(s *store.Service) {
		_, werr := s.Facts().WriteFact(ctx, "agent/a", "kb/second.md",
			originFactBody("second"), "second", "")
		require.NoError(t, werr)
		res, rerr := s.AdvanceLocalUpstream(ctx, "agent/a", "main")
		require.NoError(t, rerr)
		require.Equal(t, store.ModeFF, res.Mode)
	}))

	// B's periodic sync — one synchronous reconcile under DisableBackgroundSync.
	require.NoError(t, riB.ActivateSync(originURL))
	require.NoError(t, riB.WithRead(func(s *store.Service) {
		f, rerr := s.Facts().ReadFact(ctx, "main", "kb/second.md", nil)
		require.NoError(t, rerr)
		require.Contains(t, f.Content, "second")
	}))
}

// What a peer SEES of A through the mounted endpoint: the consensus branch as
// HEAD and the agent branches, and nothing else. This is the curated view
// arriving at a real client through GitRemoteHandler rather than at the
// store's own handler — the hidden-ref cases, which need private refs planted
// in the store, are pinned in internal/store's advertisement test.
func TestKnomitOrigin_PeerSeesOnlyTheConsensusAndAgentBranches(t *testing.T) {
	ctx := context.Background()
	a := newPeerManager(t, "agent/a")
	riA, err := a.Create(ctx, repos.CreateSpec{Name: "kb", Mode: "preset"}, nil)
	require.NoError(t, err)
	require.NoError(t, riA.WithRead(func(s *store.Service) {
		_, werr := s.Facts().WriteFact(ctx, "agent/a", "kb/first.md",
			originFactBody("first"), "first", "")
		require.NoError(t, werr)
		_, rerr := s.AdvanceLocalUpstream(ctx, "agent/a", "main")
		require.NoError(t, rerr)
	}))

	srvA := httptest.NewServer(GitRemoteHandler(a))
	defer srvA.Close()

	b := newPeerManager(t, "agent/b")
	res, err := b.ProbeOrigin(ctx, repos.OriginSpec{URL: srvA.URL + "/kb"})
	require.NoError(t, err)
	require.True(t, res.Reachable, res.Detail)
	require.Equal(t, "main", res.UpstreamBranch)
	require.Contains(t, res.Branches, "main")
	require.Contains(t, res.Branches, "agent/a")
	require.Len(t, res.Branches, 2,
		"only the consensus branch and agent branches are served: %v", res.Branches)
}
