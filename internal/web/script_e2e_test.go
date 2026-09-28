package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/mcp"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// TestScript_RealToolsEndToEnd [T4, the real wiring]: a Manager built the way
// internal/app builds it — mcp.NewScriptTools injected into repos.Deps — runs a
// `do: script` trigger whose script learns through the REAL learn handler.
// The commit it makes is on the agent branch, signed by this store's key, and
// ends with the trailer paragraph byte for byte as the store stamps it: the
// ctx transport is proven through an unchanged handler, not a stub. Sabotage:
// drop the ScriptTools wiring in app.go (the host throws "no script tools").
func TestScript_RealToolsEndToEnd(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch:           "agent/test",
		DisableBackgroundSync: true,
		ScriptTools:           mcp.NewScriptTools(nil),
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ontology := "id: e2e\nname: E2E\ntopics:\n  tasks:\n    description: tasks\n    triggers:\n" +
		"      - name: t1\n        on: learn\n        do: script\n        script: to-out\n        match: \"tasks/in/**\"\n"
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "scripted", Mode: "custom", OntologyYAML: ontology}, nil)
	require.NoError(t, err)
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	defer release()
	ctx := context.Background()
	const branch = "agent/test"

	_, err = svc.Facts().WriteFact(ctx, branch, fact.TriggerScriptPath("to-out"),
		`knomit.learn({topic: "tasks", category: "out", title: "Written by a script for " + change.path, body: "from " + change.commit, confidence: 0.8, sources: 1});`,
		"script: to-out", "updated")
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond) // let the dispatcher see the script before the fire
	firing, err := svc.Facts().WriteFact(ctx, branch, "kb/tasks/in/a.md",
		"---\ntype: observation\nconfidence: 0.8\nsources: 1\n---\n# a\n\nbody\n", "learn: a", "learn")
	require.NoError(t, err)

	var scriptCommit string
	require.Eventually(t, func() bool {
		h, err := svc.Branches().HeadCommit(ctx, branch)
		if err != nil || h == firing.CommitHash {
			return false
		}
		info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(h))
		if err != nil || !strings.Contains(info.Message, "Knomit-Trigger: t1") {
			return false
		}
		scriptCommit = h
		return true
	}, 20*time.Second, 25*time.Millisecond, "the script's commit never landed")

	info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(scriptCommit))
	require.NoError(t, err)
	want := "learn: trigger:t1\n\nKnomit-Trace: " + firing.CommitHash + "\nKnomit-Cause: " + firing.CommitHash + "\nKnomit-Trigger: t1\n"
	require.Equal(t, want, info.Message, "the real learn handler's commit carries the paragraph the store stamps")
	require.Equal(t, "test", info.AuthorName, "authored as this machine's agent branch")
	require.Equal(t, []plumbing.Hash{plumbing.NewHash(firing.CommitHash)}, info.Parents, "on the agent branch, right after the firing commit")
	signer, err := svc.Triggers().CommitSignerOf(ctx, plumbing.NewHash(scriptCommit))
	require.NoError(t, err)
	require.Equal(t, svc.SignerFingerprint(), signer.Fingerprint, "signed by this store's key")
	require.Equal(t, firing.CommitHash, store.TrailerValue(info.Message, store.TrailerCause))

	paths, err := svc.Facts().ListAll(ctx, branch)
	require.NoError(t, err)
	var out []string
	for _, p := range paths {
		if strings.HasPrefix(p, "kb/tasks/out/") {
			out = append(out, p)
		}
	}
	require.Len(t, out, 1, "the learned fact exists on the branch: %v", paths)
}
