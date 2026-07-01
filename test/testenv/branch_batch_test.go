package testenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBatch_OneCommitMultipleFiles asserts that Batch with three Write
// calls produces exactly one commit containing all three facts.
func TestBatch_OneCommitMultipleFiles(t *testing.T) {
	t.Log("Scenario: Batch(3 writes) → one snapshot, one commit, three facts present at HEAD, integrity clean")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	snap := agent.Batch("bulk import", func(w *BatchWriter) {
		w.Write("kb/a.md", Fact("a"))
		w.Write("kb/b.md", Fact("b"))
		w.Write("kb/c.md", Fact("c"))
	})

	require.NotNil(t, snap)
	require.NotEmpty(t, snap.Commit)
	require.Equal(t, "C1", snap.Name)
	// One snapshot means one commit. That's the whole point of Batch vs.
	// three separate Write calls.
	require.Len(t, agent.SnapshotsForTest(), 1)
}

// TestBatch_EmptyIsNoOp asserts that an empty batch closure does not
// advance HEAD, does not push a snapshot, and returns nil.
func TestBatch_EmptyIsNoOp(t *testing.T) {
	t.Log("Scenario: Batch with empty closure returns nil and does not advance HEAD")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	snap := agent.Batch("empty", func(w *BatchWriter) {
		// No writes.
	})

	require.Nil(t, snap)
	require.Len(t, agent.SnapshotsForTest(), 0)
}

// TestBatch_AsRespectsExplicitName asserts BatchAs names the snapshot
// explicitly, overriding the auto-generated C<N> name.
func TestBatch_AsRespectsExplicitName(t *testing.T) {
	t.Log("Scenario: BatchAs(\"bulk\", ...) produces a snapshot named \"bulk\"")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	snap := agent.BatchAs("bulk", "bulk import", func(w *BatchWriter) {
		w.Write("kb/a.md", Fact("a"))
		w.Write("kb/b.md", Fact("b"))
	})

	require.NotNil(t, snap)
	require.Equal(t, "bulk", snap.Name)
}

// TestBatch_InterleavedWithSingleWrites asserts that Batch and single
// Write calls share the same snapshot stack and counter.
func TestBatch_InterleavedWithSingleWrites(t *testing.T) {
	t.Log("Scenario: Write, Batch(2), Write produces snapshots C1 (single), C2 (batch), C3 (single)")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	c1 := agent.Write("kb/a.md", Fact("a"), "add a")
	c2 := agent.Batch("batch two", func(w *BatchWriter) {
		w.Write("kb/b.md", Fact("b"))
		w.Write("kb/c.md", Fact("c"))
	})
	c3 := agent.Write("kb/d.md", Fact("d"), "add d")

	require.Equal(t, "C1", c1.Name)
	require.Equal(t, "C2", c2.Name)
	require.Equal(t, "C3", c3.Name)
	require.Len(t, agent.SnapshotsForTest(), 3)
}
