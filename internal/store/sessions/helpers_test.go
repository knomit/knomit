package sessions

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// testSvc is the slice of a store Service these tests exercise: a pipeline
// index over a freshly opened session DB. The main DB (watermarks) is not
// wired; no test here touches a watermark.
type testSvc struct{ pi *PipelineIndex }

func (s *testSvc) Pipeline() *PipelineIndex { return s.pi }

// newPhaseTestService opens a fresh session DB in a temp dir.
func newPhaseTestService(t *testing.T) *testSvc {
	t.Helper()
	db, _, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &testSvc{pi: NewPipelineIndex(nil, db)}
}
