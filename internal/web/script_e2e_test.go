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

// TestScript_MoveThroughRealTools [F08 T-A8]: a script's
// knomit.learn(f, {retract: [change.path]}) through the REAL learn handler is
// ONE commit on the agent branch that adds the working copy AND deletes the
// firing fact, authored +move@ with the trigger's trailer paragraph; and
// knomit.learn([], {retract: [change.path]}) is a retract-only commit.
// Sabotage: forward only moment_name from the host → the firing fact
// survives (and learn([]) is refused) → red.
func TestScript_MoveThroughRealTools(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch:           "agent/test",
		DisableBackgroundSync: true,
		ScriptTools:           mcp.NewScriptTools(nil),
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ontology := "id: e2e\nname: E2E\ntopics:\n  tasks:\n    description: tasks\n    triggers:\n" +
		"      - name: take\n        on: learn\n        do: script\n        script: take\n        match: \"tasks/in/**\"\n" +
		"      - name: drop\n        on: learn\n        do: script\n        script: drop\n        match: \"tasks/drop/**\"\n"
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "moved", Mode: "custom", OntologyYAML: ontology}, nil)
	require.NoError(t, err)
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	defer release()
	ctx := context.Background()
	const branch = "agent/test"

	_, err = svc.Facts().WriteFact(ctx, branch, fact.TriggerScriptPath("take"),
		`knomit.learn({topic: "tasks", category: "working", title: "Took " + change.path, body: "b", confidence: 0.8, sources: 1}, {retract: [change.path]});`,
		"script: take", "updated")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, fact.TriggerScriptPath("drop"),
		`knomit.learn([], {retract: [change.path]});`, "script: drop", "updated")
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond) // let the dispatcher see the scripts before the fires

	// waitTrigger waits for the head to be a commit carrying trigger's trailer.
	waitTrigger := func(firing, trigger string) string {
		var got string
		require.Eventually(t, func() bool {
			h, err := svc.Branches().HeadCommit(ctx, branch)
			if err != nil || h == firing {
				return false
			}
			info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(h))
			if err != nil || !strings.Contains(info.Message, "Knomit-Trigger: "+trigger) {
				return false
			}
			got = h
			return true
		}, 20*time.Second, 25*time.Millisecond, "the %s script's commit never landed", trigger)
		return got
	}
	diffOf := func(h string) map[string]string {
		info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(h))
		require.NoError(t, err)
		require.Len(t, info.Parents, 1)
		rows, err := svc.Triggers().DiffFacts(ctx, info.Parents[0], plumbing.NewHash(h))
		require.NoError(t, err)
		out := map[string]string{}
		for _, r := range rows {
			out[r.Path] = r.Change
		}
		return out
	}

	body := "---\ntype: observation\nconfidence: 0.8\nsources: 1\n---\n# a\n\nbody\n"
	firing, err := svc.Facts().WriteFact(ctx, branch, "kb/tasks/in/a.md", body, "learn: a", "learn")
	require.NoError(t, err)
	move := waitTrigger(firing.CommitHash, "take")
	info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(move))
	require.NoError(t, err)
	require.Equal(t, "move: trigger:take\n\nKnomit-Trace: "+firing.CommitHash+"\nKnomit-Cause: "+firing.CommitHash+"\nKnomit-Trigger: take\n", info.Message)
	require.Contains(t, info.AuthorEmail, "+move@agents.knomit.io")
	require.Equal(t, []plumbing.Hash{plumbing.NewHash(firing.CommitHash)}, info.Parents, "ONE commit right after the firing one")
	d := diffOf(move)
	require.Len(t, d, 2, "%v", d)
	require.Equal(t, "deleted", d["kb/tasks/in/a.md"])
	for p, c := range d {
		if p != "kb/tasks/in/a.md" {
			require.True(t, strings.HasPrefix(p, "kb/tasks/working/"), p)
			require.Equal(t, "added", c)
		}
	}

	firing2, err := svc.Facts().WriteFact(ctx, branch, "kb/tasks/drop/b.md", body, "learn: b", "learn")
	require.NoError(t, err)
	drop := waitTrigger(firing2.CommitHash, "drop")
	info, err = svc.Triggers().CommitInfo(ctx, plumbing.NewHash(drop))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(info.Message, "retract: trigger:drop\n"), info.Message)
	require.Equal(t, map[string]string{"kb/tasks/drop/b.md": "deleted"}, diffOf(drop))
}
