package repos

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// F23: the guidance reader. Everything is read at ONE commit, the tip of the
// consensus branch; nothing from the agent branch, an experiment or the
// ontology the repo opened with.

const guidanceOntologyMain = `id: x
name: X
guidance:
  hypothesize: guidance/all.md
validations:
  - name: v-main
    message: Main's rule.
    rule: "true"
topics:
  forecast:
    description: F.
    guidance:
      hypothesize: guidance/forecast.md
      review: guidance/agent-only.md
`

const guidanceOntologyAgent = `id: x
name: X
guidance:
  hypothesize: guidance/agent-all.md
validations:
  - name: v-agent
    message: Agent's rule.
    rule: "true"
topics:
  forecast:
    description: F.
    guidance:
      hypothesize: guidance/forecast.md
`

type guidanceRepo struct {
	svc *store.Service
	ri  *RepoInstance
}

func newGuidanceRepo(t *testing.T) *guidanceRepo {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepoWithUpstream(context.Background(), map[string]string{}, "main", "agent/test"))
	// The open-time ontology is the AGENT branch's: the reader must not use it.
	agentOnt, err := fact.ParseOntology([]byte(guidanceOntologyAgent))
	require.NoError(t, err)
	ri := NewTestInstanceWithDeps(TestInstanceConfig{
		Name: "g", AgentBranch: "agent/test", Svc: svc, Ontology: agentOnt, OntologyRoot: "kb",
	})
	return &guidanceRepo{svc: svc, ri: ri}
}

func (g *guidanceRepo) put(t *testing.T, branch, path, content string) {
	t.Helper()
	_, err := g.svc.Facts().WriteFact(context.Background(), branch, path, content, "w "+path, "updated")
	require.NoError(t, err)
}

type warnings struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warnings) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.msgs...)
}

func captureGuidanceWarnings(t *testing.T) *warnings {
	t.Helper()
	w := &warnings{}
	restore := SetGuidanceWarnHookForTest(func(m string) {
		w.mu.Lock()
		w.msgs = append(w.msgs, m)
		w.mu.Unlock()
	})
	t.Cleanup(restore)
	return w
}

func TestConsensusGuidance_ReadsOnlyTheConsensusTip(t *testing.T) {
	g := newGuidanceRepo(t)
	w := captureGuidanceWarnings(t)
	ctx := context.Background()

	g.put(t, "main", ".knomit/ontology.yaml", guidanceOntologyMain)
	g.put(t, "main", ".knomit/guidance/all.md", "MAIN ALL\n")
	g.put(t, "main", ".knomit/guidance/forecast.md", "MAIN FORECAST\n")
	// The agent branch carries its own ontology, its own text for the same
	// path, the file main's ontology names but main lacks, and a file only
	// its own ontology names.
	g.put(t, "agent/test", ".knomit/ontology.yaml", guidanceOntologyAgent)
	g.put(t, "agent/test", ".knomit/guidance/forecast.md", "AGENT FORECAST\n")
	g.put(t, "agent/test", ".knomit/guidance/agent-only.md", "AGENT ONLY\n")
	g.put(t, "agent/test", ".knomit/guidance/agent-all.md", "AGENT ALL\n")

	snap := g.ri.ConsensusGuidance(ctx)
	require.NotNil(t, snap)
	require.Equal(t, "main", snap.Branch)
	tip, err := g.svc.Triggers().UpstreamTip(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, tip.String(), snap.Commit)
	require.Len(t, snap.ShortCommit(), 7)

	txt, ok := snap.Text("guidance/forecast.md")
	require.True(t, ok)
	require.Equal(t, "MAIN FORECAST\n", txt, "main's text, never the agent branch's")
	txt, ok = snap.Text("guidance/all.md")
	require.True(t, ok)
	require.Equal(t, "MAIN ALL\n", txt)
	_, ok = snap.Text("guidance/agent-only.md")
	require.False(t, ok, "declared on main, present only on the agent branch: skipped, never read from there")
	_, ok = snap.Text("guidance/agent-all.md")
	require.False(t, ok, "declared only by the agent branch's ontology")

	// The validations come from the tip's ontology too, never ri.Ontology().
	var names []string
	for _, v := range snap.Ontology.ValidationsFor("forecast") {
		names = append(names, v.Name)
	}
	require.Equal(t, []string{"v-main"}, names)

	// One warning for the skipped file, naming topic and path, and only once
	// for this commit however often the guidance is read.
	for range 3 {
		require.Same(t, snap, g.ri.ConsensusGuidance(ctx), "cached by commit")
	}
	msgs := w.all()
	require.Len(t, msgs, 1, "%v", msgs)
	require.Contains(t, msgs[0], ".knomit/guidance/agent-only.md")
	require.Contains(t, msgs[0], "declared by forecast")
	require.Contains(t, msgs[0], "does not exist")

	// A new commit on main (the guidance FILE edited, the ontology unchanged):
	// the next read has the new text, no restart, and the still-missing file
	// warns once more — once per (path, commit).
	g.put(t, "main", ".knomit/guidance/forecast.md", "MAIN FORECAST v2\n")
	snap2 := g.ri.ConsensusGuidance(ctx)
	require.NotSame(t, snap, snap2)
	txt, _ = snap2.Text("guidance/forecast.md")
	require.Equal(t, "MAIN FORECAST v2\n", txt)
	g.ri.ConsensusGuidance(ctx)
	require.Len(t, w.all(), 2)

	// A change on the agent branch alone changes nothing.
	g.put(t, "agent/test", ".knomit/guidance/all.md", "AGENT REWRITE\n")
	snap3 := g.ri.ConsensusGuidance(ctx)
	require.Same(t, snap2, snap3)
	txt, _ = snap3.Text("guidance/all.md")
	require.Equal(t, "MAIN ALL\n", txt)
}

func TestConsensusGuidance_UnusableFileSkippedOthersRender(t *testing.T) {
	g := newGuidanceRepo(t)
	w := captureGuidanceWarnings(t)
	g.put(t, "main", ".knomit/ontology.yaml", guidanceOntologyMain)
	g.put(t, "main", ".knomit/guidance/all.md", strings.Repeat("x", fact.MaxGuidanceBytes+1))
	g.put(t, "main", ".knomit/guidance/forecast.md", "OK\n")

	snap := g.ri.ConsensusGuidance(context.Background())
	require.NotNil(t, snap)
	_, ok := snap.Text("guidance/all.md")
	require.False(t, ok, "a 16 KiB + 1 file is skipped")
	txt, ok := snap.Text("guidance/forecast.md")
	require.True(t, ok)
	require.Equal(t, "OK\n", txt)
	var big bool
	for _, m := range w.all() {
		if strings.Contains(m, ".knomit/guidance/all.md") && strings.Contains(m, "declared by root") && strings.Contains(m, "limit") {
			big = true
		}
	}
	require.True(t, big, "%v", w.all())
}

func TestConsensusGuidance_NoOntologyOrNoBranch(t *testing.T) {
	ctx := context.Background()
	t.Run("no ontology at the tip", func(t *testing.T) {
		g := newGuidanceRepo(t)
		w := captureGuidanceWarnings(t)
		// The agent branch has one; main does not. No fallback.
		g.put(t, "agent/test", ".knomit/ontology.yaml", guidanceOntologyAgent)
		g.put(t, "agent/test", ".knomit/guidance/agent-all.md", "AGENT ALL\n")
		g.put(t, "main", "kb/x/y.md", "not an ontology")
		require.Nil(t, g.ri.ConsensusGuidance(ctx))
		require.Nil(t, g.ri.ConsensusGuidance(ctx))
		require.Len(t, w.all(), 1, "once per commit: %v", w.all())
		require.Contains(t, w.all()[0], "no ontology at main@")
	})
	t.Run("ontology at the tip does not parse", func(t *testing.T) {
		g := newGuidanceRepo(t)
		w := captureGuidanceWarnings(t)
		g.put(t, "main", ".knomit/ontology.yaml", "id: x\nname: X\ntopics: [broken\n")
		require.Nil(t, g.ri.ConsensusGuidance(ctx))
		require.Nil(t, g.ri.ConsensusGuidance(ctx))
		require.Len(t, w.all(), 1, "%v", w.all())
		require.Contains(t, w.all()[0], "does not parse")
	})
	t.Run("no consensus branch yet", func(t *testing.T) {
		g := newGuidanceRepo(t)
		w := captureGuidanceWarnings(t)
		g.svc.SetOrigin(&store.Origin{URL: "https://example.invalid/g.git", Branch: "release"})
		require.Equal(t, "release", g.svc.UpstreamBranch())
		g.put(t, "agent/test", ".knomit/ontology.yaml", guidanceOntologyAgent)
		require.Nil(t, g.ri.ConsensusGuidance(ctx))
		require.Nil(t, g.ri.ConsensusGuidance(ctx))
		require.Len(t, w.all(), 1, "%v", w.all())
		require.Contains(t, w.all()[0], "release does not exist")
	})
}
