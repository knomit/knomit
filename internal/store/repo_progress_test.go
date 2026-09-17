package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// A subscription to a knomit-served store DELIVERS the server's band-2 lines
// to the progress callback. This is the whole chain Task 1 built the server
// half of: server muxer → go-git demuxer → FetchOptions.Progress →
// progressWriter → the create job's message line.
func TestInitSubscription_ForwardsSidebandProgress(t *testing.T) {
	_, srv := servedStore(t, 3)

	var mu sync.Mutex
	var lines []string
	sub, err := Open(filepath.Join(t.TempDir(), "sub.db"))
	require.NoError(t, err)
	defer sub.Close()

	upstream, err := sub.InitSubscription(srv.URL, nil, "", func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})
	require.NoError(t, err)
	require.Equal(t, "main", upstream)

	mu.Lock()
	joined := strings.Join(lines, "")
	mu.Unlock()
	require.Contains(t, joined, "knomit: sending ", "collected progress:\n%s", joined)
	require.Contains(t, joined, "knomit: done", "collected progress:\n%s", joined)
}

// A nil progress callback is not an error and does not change the outcome —
// every caller that has no use for progress still passes nil.
func TestInitSubscription_NilProgressStillClones(t *testing.T) {
	origin, srv := servedStore(t, 2)

	sub, err := Open(filepath.Join(t.TempDir(), "sub.db"))
	require.NoError(t, err)
	defer sub.Close()

	upstream, err := sub.InitSubscription(srv.URL, nil, "", nil)
	require.NoError(t, err)
	require.Equal(t, "main", upstream)

	ctx := context.Background()
	want, err := origin.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	got, err := sub.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// HasObject is the layer-2 primitive: true for a commit this store holds,
// false for one it does not, and false (not a panic, not a crash) for input
// that is not a hash at all.
func TestHasObject(t *testing.T) {
	svc, _ := servedStore(t, 2)
	ctx := context.Background()

	tip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	root, err := svc.RootCommit(ctx, "main")
	require.NoError(t, err)
	require.NotEqual(t, tip, root, "the fixture must have more than one commit")

	require.True(t, svc.HasObject(tip), "the tip is held")
	require.True(t, svc.HasObject(root), "the root is held")
	require.True(t, svc.HasObject(strings.ToUpper(tip)), "hex case is not identity")

	// A well-formed hash this store does not hold.
	absent := "0123456789abcdef0123456789abcdef01234567"
	require.NotEqual(t, tip, absent)
	require.False(t, svc.HasObject(absent))

	// Shapes that are not hashes at all. NewHash zero-fills whatever it cannot
	// parse, so "nope" would otherwise be asked about as the zero hash.
	for _, bad := range []string{"", "nope", "zz23456789abcdef0123456789abcdef01234567", tip[:10]} {
		require.False(t, svc.HasObject(bad), "unparseable input must be a miss: %q", bad)
	}
}
