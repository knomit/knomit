package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// The commit summary says, per path, how the repo's `conflicts` setting (or
// the caller) settled a conflict, and says it plainly when the EXPERIMENT's
// change was dropped: the experiment is deleted on success, so a summary that
// only said "merged" would hide a loss nobody chose.
//
// SABOTAGE S1e: drop the DROPPED wording → red.
func TestSettledSummary_SaysWhatWasDropped(t *testing.T) {
	require.Empty(t, settledSummary(nil), "nothing settled: nothing appended")

	s := settledSummary([]store.SettledPath{
		{Path: "kb/a.md", Kept: "merged"},
		{Path: "kb/b.md", Kept: "dst", Dropped: "src-modify"},
		{Path: "kb/c.md", Kept: "dst", Dropped: "src-modify", Deleted: true},
		{Path: "kb/d.md", Kept: "src", Dropped: "dst-add"},
		{Path: "notes.txt", Kept: "src", Dropped: "dst-modify", Chosen: true},
	})
	require.Contains(t, s, "5 conflicting path(s) were settled")
	require.Contains(t, s, "kb/a.md: field-merged")
	require.Contains(t, s, "kb/b.md: kept the parent's version; the experiment's edit was DROPPED")
	require.Contains(t, s, "kb/c.md: the parent's deletion was kept; the experiment's edit was DROPPED")
	require.Contains(t, s, "kb/d.md: kept the experiment's version; the parent's addition was DROPPED")
	require.Contains(t, s, "notes.txt: your resolution: the experiment's version")
}
