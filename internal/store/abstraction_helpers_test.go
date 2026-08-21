package store

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/embeddings/params"
)

// openAbstractionTestService opens a fresh repo with a stub embedder, ready for
// abstraction-axis tests.
func openAbstractionTestService(t *testing.T) *Service {
	t.Helper()
	return openAbstractionTestServiceAt(t, filepath.Join(t.TempDir(), "k.db"), &stub768Embedder{})
}

// openAbstractionTestServiceAt opens (or reopens) a repo at path under emb.
// A repo that already exists is opened rather than initialised, so a test can
// reopen the same file under a different embedder.
func openAbstractionTestServiceAt(t *testing.T, path string, emb BatchEmbedder) *Service {
	t.Helper()
	_, statErr := os.Stat(path)
	existed := statErr == nil
	svc, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetEmbedder(emb)
	if existed {
		require.NoError(t, svc.OpenRepo())
	} else {
		require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	}
	return svc
}

func writeTestFact(t *testing.T, svc *Service, path, title, body string) {
	t.Helper()
	_, err := svc.Facts().WriteFact(context.Background(), "agent/test", path,
		"---\ntype: observation\n---\n# "+title+"\n\n"+body, "write "+path, "test")
	require.NoError(t, err)
}

// unitVectorAt returns the dim-dimensional basis vector e_i.
func unitVectorAt(i, dim int) []float32 {
	v := make([]float32, dim)
	v[i] = 1
	return v
}

// ringVector places point i of n on a circle in the first two dimensions, so
// cosine similarity between two points falls off with their index distance —
// which gives a KNN test a known expected order.
func ringVector(i, n, dim int) []float32 {
	v := make([]float32, dim)
	angle := 2 * math.Pi * float64(i) / float64(4*n) // spread over a quarter turn
	v[0] = float32(math.Cos(angle))
	v[1] = float32(math.Sin(angle))
	return v
}

// otherIDEmbedder is stub768Embedder under a different model id, for testing
// the embedding-identity heal.
type otherIDEmbedder struct{ stub768Embedder }

func (e *otherIDEmbedder) ID() string { return "stub768-other" }

func (e *otherIDEmbedder) Thresholds() params.Thresholds { return params.Defaults() }
