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

// A citation whose target was RETRACTED locally but is still reachable by
// walk-back must survive replay.
//
// resolveDeadRefs judged "dead" against the LIVE fact lists alone, so a merely
// retracted target counted as dead: the citation was deleted and the target's
// external refs grafted onto the citing fact in its place. That is the
// current-state question asked of a historical graph — the referrer said "I
// derive from that" at its own commit, resolveTargetCommit still resolves an
// edge to the target's last valid blob, and a later retraction does not unsay
// any of it.
func TestResolveDeadRefs_PreservesRetractedButReachableTarget(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	const target = "kb/target.md"
	_, err = svc.Facts().WriteFact(ctx, "main", target,
		testFactBody("target", 0.9, []string{"https://example.com/evidence"}), "init", "")
	require.NoError(t, err)
	_, err = svc.Facts().DeleteFact(ctx, "main", target, "retract")
	require.NoError(t, err)

	reachable, err := svc.FactQuery().FactExistsAt(ctx, "main", target, "")
	require.NoError(t, err)
	require.True(t, reachable, "precondition: a retracted fact stays reachable by walk-back")

	content := testFactBody("citing", 0.8, []string{target})
	out, resolved, dropped, err := resolveDeadRefs(
		ctx, svc, "main", content, "kb/citing.md",
		map[string]bool{}, map[string]bool{}, "") // neither LIVE set holds the target
	require.NoError(t, err)

	require.Zero(t, dropped, "a reachable target is not a dead ref")
	require.Zero(t, resolved, "nothing to graft — the citation stands on its own")
	require.Equal(t, content, out, "content must come back byte-identical")

	parsed, err := fact.ParseFact("kb/citing.md", out)
	require.NoError(t, err)
	require.Contains(t, parsed.Refs, target, "the citation must survive")
	require.NotContains(t, parsed.Refs, "https://example.com/evidence",
		"the target's own refs must NOT be grafted on — that rewrites what the citing fact said")
}

// The dead-ref path still fires for a target with no version anywhere in
// history: leniency must not degrade into "keep everything".
func TestResolveDeadRefs_StillDropsTargetThatNeverExisted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	content := testFactBody("citing", 0.8, []string{"kb/never-existed.md"})
	out, _, dropped, err := resolveDeadRefs(
		ctx, svc, "main", content, "kb/citing.md",
		map[string]bool{}, map[string]bool{}, "")
	require.NoError(t, err)
	require.Equal(t, 1, dropped, "a path with no version anywhere is genuinely dead")

	parsed, err := fact.ParseFact("kb/citing.md", out)
	require.NoError(t, err)
	require.NotContains(t, parsed.Refs, "kb/never-existed.md")
}
