package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// reconcile runs one local reconcile round: on a repo with no origin the
// consensus branch fast-forwards to the agent tip (AdvanceLocalUpstream, the
// 30 s loop's body).
func reconcile(t *testing.T, ri *repos.RepoInstance) {
	t.Helper()
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		rem, err := svc.Remote().GetRemote("origin")
		require.NoError(t, err)
		require.Nil(t, rem, "the case under test is a repo with no origin")
		_, err = svc.AdvanceLocalUpstream(context.Background(), "agent/test", svc.UpstreamBranch())
		require.NoError(t, err)
	}))
}

func onConsensus(t *testing.T, ri *repos.RepoInstance, path string) (string, bool) {
	t.Helper()
	var out string
	var ok bool
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		f, err := svc.Facts().ReadFact(context.Background(), svc.UpstreamBranch(), path, nil)
		if err == nil {
			out, ok = f.Content, true
		}
	}))
	return out, ok
}

// TestThreeRoots_F23Case_NothingReachesConsensus is the design's F23 case
// (T9, MCP half) together with T13: on a repo with no origin — where the
// local reconcile makes the agent branch main by itself — a skill a person
// committed reaches main through git and knomit_skill serves it; then every
// MCP fact door tries to add a guidance file and a bundled skill file, and to
// rewrite or delete the skill's own file. After a reconcile round, the
// consensus tip carries none of it, while a legitimate kb fact written in the
// same window did arrive (so the round really ran), and knomit_skill still
// serves exactly what the person wrote.
func TestThreeRoots_F23Case_NothingReachesConsensus(t *testing.T) {
	f := newSkillFixture(t)
	ri := f.beta // upstream "main", no origin
	ctx := repoCtx(ri)

	// A person's commit, as git would deliver it, carried to main.
	putSkill(t, ri, "agent/test", "work-task", "Do a task.", "Follow the queue.\n")
	putFile(t, ri, "agent/test", "work-task", "ref.md", "REF\n")
	reconcile(t, ri)
	_, ok := onConsensus(t, ri, ".knomit/skills/work-task/ref.md")
	require.True(t, ok, "fixture: the person's skill is on main")

	before := headOf(t, ri, "agent/test")
	for _, p := range []string{".knomit/guidance/x.md", ".knomit/skills/work-task/extra.md"} {
		r := learnAtPath(t, ctx, p, "Guidance", "INJECTED")
		require.Truef(t, r.IsError, "learn %s", p)
		require.Containsf(t, resultText(t, r), "closed to the fact tools", "learn %s", p)
	}
	for _, p := range []string{".knomit/skills/work-task/ref.md", ".knomit/skills/work-task/skill.md"} {
		r := callTool(t, UpdateHandler(), ctx, map[string]any{"file": p, "moment_name": "m", "updates": map[string]any{"body": "INJECTED"}})
		require.Truef(t, r.IsError, "update %s", p)
		require.Containsf(t, resultText(t, r), "closed to the fact tools", "update %s", p)
		r = callTool(t, RetractHandler(), ctx, map[string]any{"file": p, "moment_name": "m"})
		require.Truef(t, r.IsError, "retract %s", p)
		require.Containsf(t, resultText(t, r), "closed to the fact tools", "retract %s", p)
		r = callTool(t, LearnHandler(), ctx, map[string]any{
			"moment_name": "m",
			"facts":       []any{},
			"retract":     []any{p},
		})
		require.Truef(t, r.IsError, "learn retract %s", p)
		require.Containsf(t, resultText(t, r), "closed to the fact tools", "learn retract %s", p)
	}
	require.Equal(t, before, headOf(t, ri, "agent/test"), "no door moved the agent tip")

	// A legitimate write in the same window, then one reconcile round.
	r := callTool(t, LearnHandler(), ctx, map[string]any{
		"moment_name": "m",
		"facts": []any{map[string]any{"topic": "architecture", "category": "x/y", "title": "Legit",
			"body": "b", "confidence": 0.5, "sources": 1}},
	})
	require.False(t, r.IsError, resultText(t, r))
	reconcile(t, ri)
	require.NotEqual(t, before, headOf(t, ri, "agent/test"))

	for _, p := range []string{".knomit/guidance/x.md", ".knomit/skills/work-task/extra.md"} {
		_, ok := onConsensus(t, ri, p)
		require.Falsef(t, ok, "%s must not be on the consensus tip", p)
	}
	ref, ok := onConsensus(t, ri, ".knomit/skills/work-task/ref.md")
	require.True(t, ok)
	require.Equal(t, "REF\n", ref, "the person's bundled file is untouched")

	// knomit_skill — knomit reading .knomit/ by name, not a fact tool — still
	// serves the skill, with exactly the person's files.
	srv := NewServer("kb", f.m, false, nil)
	text, isErr := callSkill(t, srv, ctx, `{"name":"work-task"}`)
	require.False(t, isErr, text)
	var out skillGetResponse
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	require.Len(t, out.Files, 1, text)
	require.Equal(t, "ref.md", out.Files[0].Path)
	require.Equal(t, "REF\n", out.Files[0].Text)
	require.NotContains(t, text, "INJECTED")
}
