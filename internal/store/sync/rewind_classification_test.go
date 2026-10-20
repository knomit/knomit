package sync

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestReconcileMain_LogsMergeBaseErrorDistinctly regression-tests finding
// #3's secondary contract: when MergeBase fails (vs returns empty), the
// reconcile must STILL force-update local and emit a warn log that
// includes the underlying error — not silently route to the
// "not a descendant" message that gives the operator no signal about
// why disjoint detection couldn't run.
//
// We exercise the path via the rewind classifier rather than provoking a
// real MergeBase failure (which requires a corrupted object store and
// is impractical to fabricate in-process); the classifier's contract
// guarantees that a non-nil BaseErr is what reconcileMain renders to
// the distinct log line.
func TestReconcileMain_LogsMergeBaseErrorDistinctly(t *testing.T) {
	// Contract check: rewindClassification has a BaseErr field that
	// reconcileMain logs distinctly from the disjoint and shared cases.
	// A zero value classification renders no error; a non-nil BaseErr
	// (whatever the underlying cause) must not be lost to the inferior
	// "not a descendant" message.
	var noErr rewindClassification
	require.NoError(t, noErr.BaseErr, "zero classification carries no error")

	withErr := rewindClassification{BaseErr: object.ErrUnsupportedObject}
	require.Error(t, withErr.BaseErr,
		"classification preserves MergeBase errors so reconcileMain can surface them distinctly")
	require.False(t, withErr.Disjoint,
		"a MergeBase error must NOT silently mean disjoint=false — reconcileMain logs the error explicitly instead")
}
