package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// zeroForMarkerEmbedder returns a zero (degenerate) vector for any document
// whose text contains "ZEROVEC", and a normal stub vector otherwise. A
// zero-norm vector has an undefined cosine distance, which sqlite-vec returns
// as NULL — reproducing the production failure where a KNN neighbor with a
// degenerate embedding crashes the similarity-edge scan.
type zeroForMarkerEmbedder struct{ stub768Embedder }

func (e *zeroForMarkerEmbedder) EmbedDocument(title, body string) ([]float32, error) {
	if strings.Contains(title+" "+body, "ZEROVEC") {
		return make([]float32, 768), nil
	}
	return e.stub768Embedder.EmbedDocument(title, body)
}

func (e *zeroForMarkerEmbedder) EmbedDocuments(titles, bodies []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		v, _ := e.EmbedDocument(titles[i], bodies[i])
		out[i] = v
	}
	return out, nil
}

// A KNN neighbor with a NULL cosine distance (degenerate/zero-norm embedding)
// must be skipped, not abort the whole similarity-edge build for the fact.
func TestGraphBuildSimilarityEdges_SkipsNeighborWithNullDistance(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))
	svc.SetEmbedder(&zeroForMarkerEmbedder{})

	ctx := context.Background()
	const branch = "agent/a"

	// Neighbor fact with a degenerate (zero) embedding → NULL cosine distance.
	writeSrcFact(t, svc, branch, "kb/x/zero.md", "ZEROVEC degenerate body", nil, nil)
	// Source fact with a normal embedding; its KNN neighborhood includes the
	// degenerate fact above.
	writeSrcFact(t, svc, branch, "kb/x/src.md", "normal content body", nil, nil)

	var blobHash string
	require.NoError(t, svc.si.rh.db.QueryRow(
		`SELECT blob_hash FROM facts WHERE path = ?`, "kb/x/src.md").Scan(&blobHash))

	// Before the fix this returns:
	//   "scan knn row: sql: Scan error ... converting NULL to float64 is unsupported"
	err = svc.si.graphBuildSimilarityEdges(ctx, "kb/x/src.md", blobHash)
	require.NoError(t, err, "NULL-distance neighbors must be skipped, not fail the edge build")
}
