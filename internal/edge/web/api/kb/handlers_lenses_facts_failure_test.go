package kb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/index"
)

// searchRepo hands an embedder's EMPTY vector (with a nil error) back as an
// error, exactly like an embed error: leaving QueryVec empty would make the
// store embed the same text again.
func TestSearchRepo_EmptyVectorIsAnError(t *testing.T) {
	emb := &cosEmbedder{}
	emb.emptyQuery.Store(true)
	m, _ := newRealStoreManagerWith(t, emb, "alpha")

	res, err := searchRepo(context.Background(), m.Get("alpha"), emb, "agent/test", index.SearchOptions{Text: "x"})

	require.Error(t, err, "got results=%v", res)
	require.Contains(t, err.Error(), "empty vector")
	require.EqualValues(t, 1, emb.queries.Load(), "no store-side retry of an unusable embed")
}
