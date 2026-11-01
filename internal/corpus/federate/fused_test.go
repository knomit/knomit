package federate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/vocab"
)

// fusedFixture is a three-mount binding with the WRITE repo in the middle of
// binding order: alpha (0), mid (1, write), zulu (2). The instances have no
// store; every test supplies its own legs.
func fusedFixture(t *testing.T) (*repos.Binding, [3]*repos.RepoInstance) {
	t.Helper()
	inst := func(name string) *repos.RepoInstance {
		return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: name, AgentBranch: "agent/test"})
	}
	alpha, mid, zulu := inst("alpha"), inst("mid"), inst("zulu")
	b := repos.NewBindingForTest(mid,
		repos.ReadTarget{RI: alpha, Branch: "agent/test"},
		repos.ReadTarget{RI: mid, Branch: "agent/test"},
		repos.ReadTarget{RI: zulu, Branch: "agent/test"},
	)
	return b, [3]*repos.RepoInstance{alpha, mid, zulu}
}

// searchLegOf answers each mount's list from rows, keyed by mount name.
func searchLegOf(rows map[string][]string) SearchLeg {
	return func(_ context.Context, t Target, _ index.SearchOptions) ([]index.SearchResult, error) {
		var out []index.SearchResult
		for _, p := range rows[t.RT.RI.Name()] {
			var r index.SearchResult
			r.Path = p
			r.Title = t.RT.RI.Name() + ":" + p
			out = append(out, r)
		}
		return out, nil
	}
}

func hitTitles[T any](f *Fused[T], title func(T) string) []string {
	out := make([]string, len(f.Hits))
	for i, h := range f.Hits {
		out[i] = title(h.Row)
	}
	return out
}

// Ported from internal/edge/mcp's keepWinners test, now against the shared
// function: a path on the write mount AND a read mount is listed once, as the
// write mount's copy, even when the read mount's copy ranks first; the order of
// the survivors is the RRF order (rank, then mount index).
func TestFusedSearch_RRFOrderAndWriteFirstDedupe(t *testing.T) {
	b, _ := fusedFixture(t)
	plan, err := PlanFused(context.Background(), FusedQuery{Binding: b})
	require.NoError(t, err)
	res, err := FusedSearch(context.Background(), plan, searchLegOf(map[string][]string{
		"alpha": {"kb/shared.md", "kb/a.md"},
		"mid":   {"kb/m.md", "kb/shared.md"},
		"zulu":  {"kb/z1.md", "kb/z2.md"},
	}), FusedOptions{})
	require.NoError(t, err)
	require.Equal(t, []string{
		// rank 0: alpha's shared copy is shadowed by mid's (write first)
		"mid:kb/m.md", "zulu:kb/z1.md",
		// rank 1: alpha, mid (the winning shared copy), zulu
		"alpha:kb/a.md", "mid:kb/shared.md", "zulu:kb/z2.md",
	}, hitTitles(res, func(r index.SearchResult) string { return r.Title }))
	require.Equal(t, []int{2, 2, 2}, res.ListLens())
	for _, h := range res.Hits {
		require.Equal(t, plan.Targets[h.Mount], h.Target, "a hit carries the target it came from")
	}
}

// The reported failure is the LOWEST-indexed failing mount, never the first to
// finish: mid (1) fails last, zulu (2) fails first.
func TestFusedSearch_LowestIndexedFailureWins(t *testing.T) {
	b, _ := fusedFixture(t)
	plan, err := PlanFused(context.Background(), FusedQuery{Binding: b})
	require.NoError(t, err)
	_, err = FusedSearch(context.Background(), plan,
		func(_ context.Context, t Target, _ index.SearchOptions) ([]index.SearchResult, error) {
			switch t.RT.RI.Name() {
			case "mid":
				time.Sleep(100 * time.Millisecond)
				return nil, errors.New("mid down")
			case "zulu":
				return nil, errors.New("zulu down")
			}
			return nil, nil
		}, FusedOptions{})
	var f *MountError
	require.ErrorAs(t, err, &f)
	require.Equal(t, 1, f.Mount)
	require.EqualError(t, err, "mid down")
	require.False(t, f.Panicked)
}

// A panicking leg becomes that mount's error (concurrent mode), and its label
// is built without dereferencing a nil RepoInstance.
func TestFanOut_PanicBecomesTheMountsErrorEvenWithNoInstance(t *testing.T) {
	targets := []Target{{RT: repos.ReadTarget{RI: nil, Branch: "gone"}}}
	f := FanOut(targets, func(int, Target) error { panic("boom") })
	require.NotNil(t, f)
	require.True(t, f.Panicked)
	require.EqualError(t, f, "mount gone panicked: boom")
}

// Serial mode reads in order, stops at the first failure and does not recover
// a panic.
func TestFused_SerialStopsAtTheFirstFailureAndDoesNotRecover(t *testing.T) {
	b, _ := fusedFixture(t)
	plan, err := PlanFused(context.Background(), FusedQuery{Binding: b})
	require.NoError(t, err)
	var calls atomic.Int64
	_, err = FusedRecent(context.Background(), plan,
		func(_ context.Context, t Target, _ index.SearchOptions) ([]index.RecentFactEntry, int, error) {
			calls.Add(1)
			if t.RT.RI.Name() == "mid" {
				return nil, 0, errors.New("mid down")
			}
			return nil, 0, nil
		}, FusedOptions{Serial: true})
	var f *MountError
	require.ErrorAs(t, err, &f)
	require.Equal(t, 1, f.Mount)
	require.EqualValues(t, 2, calls.Load(), "zulu is never read after mid failed")

	require.PanicsWithValue(t, "boom", func() {
		_, _ = FusedRecent(context.Background(), plan,
			func(context.Context, Target, index.SearchOptions) ([]index.RecentFactEntry, int, error) {
				panic("boom")
			}, FusedOptions{Serial: true})
	})
}

// The recency listing merges by committed_at without text and fuses by rank
// with text; Totals carries each leg's own count.
func TestFusedRecent_MergeWithoutTextRRFWithText(t *testing.T) {
	b, _ := fusedFixture(t)
	rows := map[string][]index.RecentFactEntry{
		"alpha": {{Path: "kb/a1.md", CommittedAt: 30}, {Path: "kb/a2.md", CommittedAt: 10}},
		"mid":   {{Path: "kb/m1.md", CommittedAt: 20}},
		"zulu":  {{Path: "kb/z1.md", CommittedAt: 40}, {Path: "kb/a1.md", CommittedAt: 5}},
	}
	leg := func(_ context.Context, t Target, _ index.SearchOptions) ([]index.RecentFactEntry, int, error) {
		return rows[t.RT.RI.Name()], 7, nil
	}
	paths := func(f *Fused[index.RecentFactEntry]) []string {
		return hitTitles(f, func(e index.RecentFactEntry) string { return e.Path })
	}

	plan, err := PlanFused(context.Background(), FusedQuery{Binding: b})
	require.NoError(t, err)
	res, err := FusedRecent(context.Background(), plan, leg, FusedOptions{})
	require.NoError(t, err)
	// committed_at DESC; zulu's kb/a1.md loses to alpha's (binding order among reads)
	require.Equal(t, []string{"kb/z1.md", "kb/a1.md", "kb/m1.md", "kb/a2.md"}, paths(res))
	require.Equal(t, []int{7, 7, 7}, res.Totals)

	plan, err = PlanFused(context.Background(), FusedQuery{Binding: b, Opts: index.SearchOptions{Text: "x"}})
	require.NoError(t, err)
	res, err = FusedRecent(context.Background(), plan, leg, FusedOptions{})
	require.NoError(t, err)
	// by rank, then mount index
	require.Equal(t, []string{"kb/a1.md", "kb/m1.md", "kb/z1.md", "kb/a2.md"}, paths(res))
}

// PlanFused runs its steps in order and names the failing one; a step after
// the failure does not run. The query is embedded once.
func TestPlanFused_StagesInOrder(t *testing.T) {
	b, mounts := fusedFixture(t)
	var embeds atomic.Int64
	embed := func(_ context.Context, text string) ([]float32, error) {
		embeds.Add(1)
		if text == "fail" {
			return nil, errors.New("embedder down")
		}
		return []float32{1}, nil
	}
	stage := func(err error) FusedStage {
		var pe *PlanError
		require.ErrorAs(t, err, &pe)
		return pe.Stage
	}

	_, err := PlanFused(context.Background(), FusedQuery{Binding: b, Path: "kb://000000000000/kb/", Repos: []string{"nope"}, Embed: embed})
	require.Equal(t, StageTargets, stage(err))
	require.EqualError(t, err, "repo 000000000000 is not mounted in this binding")

	_, err = PlanFused(context.Background(), FusedQuery{Binding: b, Repos: []string{"alpha", "nope"}, Embed: embed})
	require.Equal(t, StageRepos, stage(err))
	require.EqualError(t, err, `no mount named "nope" in lens "mid"`)
	require.Zero(t, embeds.Load(), "no embedding after a planning failure")

	_, err = PlanFused(context.Background(), FusedQuery{Binding: b, Opts: index.SearchOptions{Text: "fail"}, Embed: embed})
	require.Equal(t, StageEmbed, stage(err))

	plan, err := PlanFused(context.Background(), FusedQuery{Binding: b, Repos: []string{"zulu", "alpha"}, Opts: index.SearchOptions{Text: "x"}, Embed: embed})
	require.NoError(t, err)
	require.Len(t, plan.Targets, 2)
	require.Same(t, mounts[0], plan.Targets[0].RT.RI, "narrowing keeps binding order")
	require.Same(t, mounts[2], plan.Targets[1].RT.RI)
	require.Equal(t, []float32{1}, plan.Opts.QueryVec)
	require.EqualValues(t, 2, embeds.Load())

	readErr := errors.New("vocabulary down")
	_, err = PlanFused(context.Background(), FusedQuery{Binding: b, Opts: index.SearchOptions{Motifs: []string{"a-b"}},
		Clusters: func(context.Context, repos.ReadTarget) ([]vocab.MotifCluster, error) { return nil, readErr }})
	require.Equal(t, StageMotifs, stage(err))
	require.ErrorIs(t, err, readErr)
}
