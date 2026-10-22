package git

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func blobObj(t *testing.T, s *Storer, content string) plumbing.EncodedObject {
	t.Helper()
	o := s.NewEncodedObject()
	o.SetType(plumbing.BlobObject)
	w, err := o.Writer()
	require.NoError(t, err)
	_, err = w.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return o
}

func countObjects(t *testing.T, s *Storer) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM objects`).Scan(&n))
	return n
}

// All-or-nothing: a batch containing an object the storer refuses writes
// NONE of the batch, not the prefix before it.
func TestPutObjects_AllOrNothing(t *testing.T) {
	s, err := NewMemoryStorer()
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	a, b := blobObj(t, s, "a"), blobObj(t, s, "b")
	require.NoError(t, s.PutObjects(ctx, []plumbing.EncodedObject{a, b}))
	require.Equal(t, 2, countObjects(t, s))
	require.NoError(t, s.HasEncodedObject(a.Hash()))

	// Re-putting present objects is not an error (INSERT OR IGNORE).
	require.NoError(t, s.PutObjects(ctx, []plumbing.EncodedObject{a}))
	require.Equal(t, 2, countObjects(t, s))

	c := blobObj(t, s, "c")
	bad := &plumbing.MemoryObject{}
	bad.SetType(plumbing.REFDeltaObject)
	require.Error(t, s.PutObjects(ctx, []plumbing.EncodedObject{c, bad}))
	require.Equal(t, 2, countObjects(t, s), "a refused batch must write nothing")
	require.ErrorIs(t, s.HasEncodedObject(c.Hash()), plumbing.ErrObjectNotFound)
}

// A cancelled batch rolls back: the context is honoured and nothing lands.
func TestPutObjects_CancelledWritesNothing(t *testing.T) {
	s, err := NewMemoryStorer()
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, s.PutObjects(ctx, []plumbing.EncodedObject{blobObj(t, s, "x")}))
	require.Equal(t, 0, countObjects(t, s))
}
