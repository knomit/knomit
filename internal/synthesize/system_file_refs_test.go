package synthesize

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// #428 follow-up (review §6): the synthesize gates resolve refs to .knomit/
// files. Three writers can meet a NEW .knomit/ ref, because the model writes
// their refs: reflect (all-or-nothing), distill (warn and skip that fact) and
// the prune merge (warn and skip that merge). Each accepts a ref to a file in
// the tree at the tip of the branch the write lands on, stored canonical with
// case kept, and refuses one to a file that is missing there, present only on
// another branch, or a symlink (never followed).

const (
	sfRepoID   = "0123456789ab"
	sfFile     = ".knomit/skills/program-knomit/SKILL.md" // on both branches, case kept
	sfSideOnly = ".knomit/runs/side-only.txt"             // only on the session branch
	sfMainOnly = ".knomit/runs/main-only.txt"             // only on the OTHER branch
	sfMissing  = ".knomit/runs/gone.txt"                  // nowhere
	sfLink     = ".knomit/skills/link.md"                 // symlink -> program-knomit/SKILL.md
	sfMain     = "agent/test"
	sfSide     = "agent/side" // the branch every write below lands on
)

var (
	sfGood = []string{sfFile, sfSideOnly}
	sfBad  = []string{sfMainOnly, sfMissing, sfLink}
)

func systemFileSynthRepo(t *testing.T) *store.Service {
	t.Helper()
	ctx := context.Background()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(ctx, map[string]string{sfFile: "---\nname: program-knomit\n---\nskill\n"}, sfMain))
	require.NoError(t, svc.Branches().CreateBranch(ctx, sfSide, sfMain))
	_, err = svc.RawWriteForTest(ctx, sfSide, sfSideOnly, "side\n", "seed")
	require.NoError(t, err)
	_, err = svc.RawWriteForTest(ctx, sfMain, sfMainOnly, "main\n", "seed")
	require.NoError(t, err)
	_, err = svc.RawSymlinkForTest(ctx, sfSide, sfLink, "program-knomit/SKILL.md", "a symlink pushed through git")
	require.NoError(t, err)
	seedDated(t, svc, sfSide, "kb/technology/a.md", fact.Hypothesis, "")
	seedDated(t, svc, sfSide, "kb/technology/b.md", fact.Observation, "")
	return svc
}

func sfCanonical(refs ...string) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = "kb://" + sfRepoID + "/" + r
	}
	return out
}

func sfTip(t *testing.T, svc *store.Service) string {
	t.Helper()
	head, err := svc.Branches().HeadCommit(context.Background(), sfSide)
	require.NoError(t, err)
	return head
}

// requireFileRefusal asserts the gate's .knomit/ section named ref — not some
// other refusal (a missing fact, "cannot check refs to .knomit/ files").
func requireFileRefusal(t *testing.T, text, ref string) {
	t.Helper()
	require.Containsf(t, text, "cites "+ref+", which does not exist", "refusal names %s: %s", ref, text)
	require.Containsf(t, text, "does not exist under .knomit/ at the tip", "the .knomit/ fix line: %s", text)
}

// TestApplyReflectDecisions_SystemFileRefs: a proposed methodology citing
// files on the session branch lands with the refs canonical; one citing a
// file only on another branch, a missing file or a symlink aborts the whole
// reflect and writes nothing.
// Sabotage: writeGate without WithFiles → the good call fails "cannot check
// refs" → red; file resolver always true → bad calls land → red; resolver on
// sfMain → sfSideOnly refused and sfMainOnly accepted → red.
func TestApplyReflectDecisions_SystemFileRefs(t *testing.T) {
	svc := systemFileSynthRepo(t)
	ctx := context.Background()
	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "review", sfSide, "")
	require.NoError(t, err)
	propose := func(title string, refs ...string) ReflectResult {
		return ReflectResult{Propose: []ProposeEntry{{
			TopicPath: "meta/reasoning", Title: title, Body: "a methodology " + title,
			NoveltyArgument: "nothing like it exists", Confidence: 0.8,
			TransitionPaths: []string{"kb/technology/a.md"},
			Refs:            append([]string{"kb/technology/a.md"}, refs...),
		}}}
	}

	for _, bad := range sfBad {
		before := sfTip(t, svc)
		err := ApplyReflectDecisions(ctx, svc.Facts(), svc.Search(), propose("bad "+bad, sfFile, bad), sess,
			sfRepoID, "kb", 0.95, nil)
		require.Errorf(t, err, "reflect citing %s", bad)
		requireFileRefusal(t, err.Error(), bad)
		require.NotContainsf(t, err.Error(), "cites "+sfFile, "the good ref is not refused alongside %s", bad)
		require.Equalf(t, before, sfTip(t, svc), "an aborted reflect writes nothing (%s)", bad)
	}

	require.NoError(t, ApplyReflectDecisions(ctx, svc.Facts(), svc.Search(), propose("good", sfGood...), sess,
		sfRepoID, "kb", 0.95, nil))
	found := methodologyFactPaths(t, svc, sfSide)
	require.Len(t, found, 1, "the reflect wrote its methodology")
	got := readFactForTest(t, svc, sfSide, found[0]).Refs
	for _, want := range sfCanonical(sfGood...) {
		require.Contains(t, got, want, "stored canonical, case kept")
	}
}

// TestApplyDistillDecisions_SystemFileRefs: a distilled fact citing files on
// the session branch is written with the refs canonical; each fact citing a
// bad one is warned and skipped, and the good one beside it still lands.
// Sabotage: as for reflect.
func TestApplyDistillDecisions_SystemFileRefs(t *testing.T) {
	svc := systemFileSynthRepo(t)
	synth := func(name string, refs ...string) distillFact {
		return distillFact{
			Path: "kb/technology/" + name + ".md", Title: "S " + name, Body: "distilled " + name, Type: "synthesis",
			Domain: []string{"technology"}, Confidence: 0.9,
			Refs: append([]string{"kb/technology/a.md", "kb/technology/b.md"}, refs...),
		}
	}
	in := []distillFact{synth("good", sfGood...)}
	for i, bad := range sfBad {
		in = append(in, synth("bad"+string(rune('a'+i)), bad))
	}
	sink, warns := collectWarns()
	_, written, err := ApplyDistillDecisions(context.Background(), svc.Facts(), svc.Search(), in, nil,
		"test", sink, sfSide, sfRepoID, "kb")
	require.NoError(t, err)
	require.Len(t, written, 1, "only the good fact lands: %v", *warns)
	good := readFactForTest(t, svc, sfSide, written[0].Path)
	require.Equal(t, "S good", good.Title)
	got := good.Refs
	for _, want := range sfCanonical(sfGood...) {
		require.Contains(t, got, want, "stored canonical, case kept")
	}
	all := strings.Join(*warns, "\n")
	for _, bad := range sfBad {
		requireFileRefusal(t, all, bad)
	}
	require.NotContains(t, all, "cites "+sfFile)
}

// TestApplyPruneDecisions_MergeSystemFileRefs: a merge whose merged fact ADDS
// a ref no member carried (so prior does not exempt it) lands when the file is
// on the session branch, and is warned and skipped otherwise.
// Sabotage: as for reflect.
func TestApplyPruneDecisions_MergeSystemFileRefs(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		good bool
	}{{sfFile, true}, {sfSideOnly, true}, {sfMainOnly, false}, {sfMissing, false}, {sfLink, false}} {
		t.Run(tc.ref, func(t *testing.T) {
			svc := systemFileSynthRepo(t)
			members := []string{"kb/technology/a.md", "kb/technology/b.md"}
			for _, m := range members {
				require.Empty(t, readFactForTest(t, svc, sfSide, m).Refs, "fixture: no member carries the ref, so prior cannot exempt it")
			}
			sink, warns := collectWarns()
			stats, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.Search(), nil, []MergeEntry{{
				Paths: members,
				Merged: mergedFact{Path: "kb/technology/m.md", Title: "Merged", Body: "merged body", Type: "observation",
					Refs: flexStrings{tc.ref}},
			}}, "review-test", sink, sfSide, sfRepoID, "kb", nil)
			require.NoError(t, err)
			if tc.good {
				require.Equal(t, 1, stats.Merged, "warns: %v", *warns)
				got := readFactForTest(t, svc, sfSide, mergedFactPath(t, svc, sfSide, "Merged")).Refs
				require.Contains(t, got, sfCanonical(tc.ref)[0], "stored canonical, case kept")
				return
			}
			require.Equal(t, 0, stats.Merged)
			requireFileRefusal(t, strings.Join(*warns, "\n"), tc.ref)
			for _, m := range members {
				readFactForTest(t, svc, sfSide, m) // the skipped merge retracted nothing
			}
		})
	}
}

// TestSynthesize_OneRefGateConstructor: every ref gate in this package is
// built by writeGate, so none can be built without the .knomit/ file
// resolver. A refs.New outside ref_gate.go fails the build here.
func TestSynthesize_OneRefGateConstructor(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	ctor := regexp.MustCompile(`\brefs\.New\(`)
	var hits []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || n == "ref_gate.go" {
			continue
		}
		src, err := os.ReadFile(n)
		require.NoError(t, err)
		if ctor.Match(src) {
			hits = append(hits, n)
		}
	}
	require.Empty(t, hits, "build synthesize ref gates with writeGate, not refs.New")
}
