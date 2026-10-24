package kb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/index"
)

// searchRepo hands the embedder's failure back as an error. Before, it logged
// and let the store re-embed (and the store then swallowed it).
func TestSearchRepo_EmbedErrorPropagates(t *testing.T) {
	emb := &cosEmbedder{}
	emb.failQuery.Store(true)
	m, _ := newRealStoreManagerWith(t, emb, "alpha")

	res, err := searchRepo(context.Background(), m.Get("alpha"), emb, "agent/test", index.SearchOptions{Text: "x"})

	require.Error(t, err, "want the embedder's error, got results=%v", res)
	require.ErrorIs(t, err, errInferenceDown)
	require.EqualValues(t, 1, emb.queries.Load(), "no store-side retry of a failed embed")
}
