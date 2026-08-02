package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"knomit/internal/fact"
)

// resolveDeadRefs kept a ref only if it started with http(s):// or its literal
// string was in the local/remote fact-path set. Everything else was treated as
// a dead LOCAL ref and, when history yielded no substitute, dropped from the
// fact and re-serialized — silently deleting every src:// citation, every
// file:/// ref, and every cross-repo kb:// pointer.
//
// Replay runs when two knomit instances with disjoint histories connect, so
// that destroyed source citations at exactly the moment two corpora merge.
// Dead-LOCAL-ref resolution is the intended behaviour and stays; it just must
// not apply to refs that were never local.
func TestResolveDeadRefs_PreservesNonLocalRefs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	const (
		srcNew    = "src://7b4887ce51d9/internal/x.go@4154e92c8ff333435fd00c442489e855e4c3331e:36b1d45187d6a2c6ad18d591142227ad2a02a66e"
		srcLegacy = "src://knomit/internal/legacy.go@ca1c272"
		foreign   = "kb://7b4887ce51d9/kb/z.md"
		external  = "https://example.com/a"
		fileRef   = "file:///tmp/b"
		liveLocal = "kb/live.md"
		deadLocal = "kb/dead.md"
	)

	content := testFactBody("subject", 0.5, []string{
		liveLocal, deadLocal, srcNew, srcLegacy, foreign, external, fileRef,
	})

	// Only the live local path is known; kb/dead.md is genuinely dead.
	localPaths := map[string]bool{liveLocal: true}
	remotePaths := map[string]bool{}

	out, resolved, dropped, err := resolveDeadRefs(
		ctx, svc, "main", content, "kb/subject.md", localPaths, remotePaths, "")
	require.NoError(t, err)

	require.Equal(t, 1, dropped, "only kb/dead.md may be dropped, not the four non-local refs")
	require.Equal(t, 0, resolved)

	parsed, err := fact.ParseFact("kb/subject.md", out)
	require.NoError(t, err)

	for _, want := range []string{liveLocal, srcNew, srcLegacy, foreign, external, fileRef} {
		require.Containsf(t, parsed.Refs, want, "ref %q must survive replay", want)
	}
	require.NotContains(t, parsed.Refs, deadLocal, "the genuinely dead local ref should be dropped")
}

// Nothing to do: a fact whose refs are all non-local must come back byte
// identical, without a re-serialize round trip.
func TestResolveDeadRefs_NoLocalRefsIsANoOp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	content := testFactBody("subject", 0.5, []string{
		"https://example.com/a",
		"src://knomit/internal/legacy.go@ca1c272",
	})

	out, resolved, dropped, err := resolveDeadRefs(
		ctx, svc, "main", content, "kb/subject.md",
		map[string]bool{}, map[string]bool{}, "")
	require.NoError(t, err)
	require.Zero(t, resolved)
	require.Zero(t, dropped)
	require.Equal(t, content, out, "content must be returned unchanged")
	require.True(t, strings.Contains(out, "src://knomit/internal/legacy.go@ca1c272"))
}
