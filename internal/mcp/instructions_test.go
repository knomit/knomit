package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
)

// TestProfileInstructions_HypothesizeStepIsPlain verifies that the per-fact
// hypothesize loop's step 5 no longer carries the misplaced bridge/discovered
// language. A single-fact, self-reasoned hypothesis is authored by default;
// the discovered origin is decided at proposal time in the discover work-item
// prompt, not in this loop. Renders cleanly across all MCP profiles.
func TestProfileInstructions_HypothesizeStepIsPlain(t *testing.T) {
	for _, profile := range []string{"code", "chat", "generic"} {
		t.Run(profile, func(t *testing.T) {
			out := ProfileInstructions(profile, "kb", nil)
			require.NotEmpty(t, out)

			require.Contains(t, out, "call knomit_learn with type: hypothesis",
				"step 5 must keep the plain hypothesis-write instruction")
			// The reverted-out phrasings must be gone — origin is not decided by
			// whether the agent previewed the fact.
			require.NotContains(t, out, "previewed before saving",
				"the review-act trigger must be removed from the instructions")
			require.NotContains(t, out, "stays origin: authored",
				"the misplaced bridge case must be removed from the hypothesize loop")
		})
	}
}

// TestProfileInstructions_LearnDescriptionKeysOffGrouping verifies the base
// knomit_learn description ties origin to how the candidate group was formed,
// not to the review act.
func TestProfileInstructions_LearnDescriptionKeysOffGrouping(t *testing.T) {
	out := ProfileInstructions("code", "kb", nil)
	// Find the knomit_learn bullet and assert it speaks of work-item grouping.
	require.Contains(t, out, "origin reflects how the candidate group was formed, not whether you previewed it")
	require.True(t, strings.Contains(out, "discovered for a cross-cluster bridge"),
		"learn description must map bridge → discovered")
}

// TestLensInstructions_LensOfOneIsEmpty verifies a single-repo binding produces
// no addendum — single-repo sessions keep byte-for-byte instructions.
func TestLensInstructions_LensOfOneIsEmpty(t *testing.T) {
	ri := newLearnTestRepo(t, ontologyWithTopic(t, "decisions"))
	lensOfOne := repos.NewBindingOfRepo(ri, "agent/test")
	require.Equal(t, "", lensInstructions(lensOfOne), "a lens-of-one is not a lens")
}

// TestBindingInstructions_LensOfOneMatchesProfileInstructions proves the
// single-repo path is byte-for-byte identical to today: for a lens-of-one the
// write repo IS the only mount, IsLens() is false, lensInstructions returns "",
// so BindingInstructions collapses to ProfileInstructions over the write repo's
// ontology.
func TestBindingInstructions_LensOfOneMatchesProfileInstructions(t *testing.T) {
	ri := newLearnTestRepo(t, ontologyWithTopic(t, "decisions"))
	lensOfOne := repos.NewBindingOfRepo(ri, "agent/test")

	got := BindingInstructions(lensOfOne, "code")
	want := ProfileInstructions("code", ri.OntologyRoot(), ri.Ontology())
	require.Equal(t, want, got, "lens-of-one instructions must be byte-for-byte ProfileInstructions")
}

// TestBindingInstructions_LensAppendsMountTable proves a federating binding
// gets the lens addendum appended after the write-repo base: the output both
// starts with the write repo's ProfileInstructions and carries the mount table.
func TestBindingInstructions_LensAppendsMountTable(t *testing.T) {
	writeRepo := newLearnTestRepo(t, ontologyWithTopic(t, "decisions"))
	readRepo := newLearnTestRepo(t, ontologyWithTopic(t, "other"))
	lens := repos.NewBindingForTest(writeRepo,
		repos.ReadTarget{RI: writeRepo, Branch: "agent/test"},
		repos.ReadTarget{RI: readRepo, Branch: "agent/test", Source: "core-src"},
	)

	got := BindingInstructions(lens, "code")
	base := ProfileInstructions("code", writeRepo.OntologyRoot(), writeRepo.Ontology())

	require.True(t, strings.HasPrefix(got, base), "lens instructions must begin with the write-repo base")
	require.Contains(t, got, id12(readRepo.ID()), "lens instructions must carry the mount table")
	require.Contains(t, got, "Federated knowledge base (lens)", "lens addendum header must be present")
}

// TestLensInstructions_BuildsMountTableAndConventions verifies the lens
// addendum: a mount-table row per mount (name, 12-hex id, branch, role,
// source), the write mount marked read+write, the kb:// qualified-path
// convention with a real read-mount example, the read-mount workflow and
// read-only rules, and each mount's topic coverage.
func TestLensInstructions_BuildsMountTableAndConventions(t *testing.T) {
	writeRepo := newLearnTestRepo(t, ontologyWithTopic(t, "decisions"))
	readRepo := newLearnTestRepo(t, ontologyWithTopic(t, "other"))
	lens := repos.NewBindingForTest(writeRepo,
		repos.ReadTarget{RI: writeRepo, Branch: "agent/test"},
		repos.ReadTarget{RI: readRepo, Branch: "agent/test", Source: "core-src"},
	)
	writeID := id12(writeRepo.ID())
	readID := id12(readRepo.ID())

	out := lensInstructions(lens)
	require.NotEmpty(t, out)

	// Mount-table rows: each mount's 12-hex id, the branch, and both roles.
	require.Contains(t, out, writeID, "write mount id in table")
	require.Contains(t, out, readID, "read mount id in table")
	require.Contains(t, out, "agent/test", "branch column")
	require.Contains(t, out, "read+write", "write mount role")
	require.Contains(t, out, "| read |", "read mount role cell")
	require.Contains(t, out, "core-src", "read mount source column")

	// kb:// qualified-path convention text (load-bearing, verbatim).
	require.Contains(t, out, "kb://<repo-id>/…")
	require.Contains(t, out, "Use the qualified path verbatim as the `file` argument to `knomit_explain`")
	// Concrete example uses a real read-mount id12.
	require.Contains(t, out, "kb://"+readID+"/")

	// Read-mount read-only rule (workflow sentence, load-bearing).
	require.Contains(t, out, "Facts from read mounts are READ-ONLY through this lens")
	require.Contains(t, out, "`knomit_update` and `knomit_retract`")

	// Per-mount topic coverage (union): each mount's distinct topics.
	require.Contains(t, out, "decisions", "write mount topic")
	require.Contains(t, out, "other", "read mount topic")
}
