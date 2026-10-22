package merge

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The trailer paragraph: appended to an existing trailer block, else a new
// last paragraph; sorted by path.
func TestAppendTrailerLines(t *testing.T) {
	require.Equal(t, "merge: a into b (x)\n\nKnomit-Conflict: a.md k\nKnomit-Merge: b.md m\n",
		AppendTrailerLines("merge: a into b (x)", []string{"Knomit-Merge: b.md m", "Knomit-Conflict: a.md k"}))
	require.Equal(t, "learn: x\n\nKnomit-Trace: t\nKnomit-Merge: f.md m\n",
		AppendTrailerLines("learn: x\n\nKnomit-Trace: t\n", []string{"Knomit-Merge: f.md m"}))
	require.Equal(t, "learn: x\n\nbody words here.\n\nKnomit-Merge: f.md m\n",
		AppendTrailerLines("learn: x\n\nbody words here.", []string{"Knomit-Merge: f.md m"}))
	require.Equal(t, "same", AppendTrailerLines("same", nil))
}
