package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// #384: a path go-git refuses to READ (ValidTreePath, applied by FindEntry,
// TreeEntryFile and the tree walker under DiffTree) was accepted on WRITE. The
// read that refuses it runs in notifyCommit's im.Sync, after the ref has
// moved, so the write committed, reported failure, and left a commit every
// later sync on the branch tripped over. Every write door must refuse such a
// path before the ref moves, by go-git's own rule.

func newInvalidPathTestStore(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "main"))
	return svc
}

func headOfBranch(t *testing.T, svc *Service, branch string) string {
	t.Helper()
	h, err := svc.Branches().HeadCommit(context.Background(), branch)
	require.NoError(t, err)
	return h
}

// unreadablePaths are paths go-git refuses to read back. Every one moved the
// ref and then failed im.Sync before the fix, except NUL, which go-git already
// refuses at tree encode.
var unreadablePaths = map[string]string{
	"line feed":       "kb/architecture/a\nb/x.md",
	"tab":             "kb/architecture/a\tb/x.md",
	"del":             "kb/architecture/a\x7fb/x.md",
	"nul":             "kb/architecture/a\x00b/x.md",
	"ntfs short name": "kb/architecture/git~1/x.md",
	"backslash dot":   `kb/architecture/a\./x.md`,
	"zwj dot git":     "kb/architecture/.‍git/x.md",
	"dot git":         "kb/architecture/.git/x.md",
	"dot component":   "kb/architecture/./x.md",
}

// readablePaths are paths the store must keep accepting: knomit's own
// dot-prefixed machinery, root files, and ordinary fact paths. A leading dot
// is not a .git disguise.
var readablePaths = []string{
	".knomit/jobs/sweep/state.md",
	".domains/ontology.yaml",
	".github/workflows/ci.yml",
	"README.md",
	"LICENSE",
	"kb/architecture/first/cat/abcd1234.md",
	"kb/architecture/with space/x.md",
	"kb/architecture/café/x.md",
	"kb/architecture/github/x.md",
	"kb/architecture/git~2/x.md",
}

func TestValidatePath_FollowsGoGitReadBackRule(t *testing.T) {
	for name, p := range unreadablePaths {
		err := validatePath(p)
		require.ErrorIs(t, err, ErrInvalidPath, name)
		require.True(t, IsInvalidPath(err), name)
	}
	for _, p := range readablePaths {
		require.NoError(t, validatePath(p), p)
	}
	require.ErrorIs(t, validatePath(""), ErrInvalidPath)
	require.ErrorIs(t, validatePath("kb/a/../b.md"), ErrInvalidPath)
}

// The sentinel is recovered from go-git's internal package by unwrapping a
// probe. If an upgrade stopped wrapping it, gitReadablePath would accept
// everything; this pins it.
func TestGitErrInvalidPath_Recovered(t *testing.T) {
	require.NotNil(t, gitErrInvalidPath)
	require.Equal(t, "invalid path", gitErrInvalidPath.Error())
}

func TestWriteFact_RefusesUnreadablePath(t *testing.T) {
	for name, path := range unreadablePaths {
		t.Run(name, func(t *testing.T) {
			svc := newInvalidPathTestStore(t)
			ctx := context.Background()
			before := headOfBranch(t, svc, "main")

			_, err := svc.Facts().WriteFact(ctx, "main", path, "body", "learn: m", "learn")
			require.Error(t, err)
			require.Equal(t, before, headOfBranch(t, svc, "main"), "a refused write must not move the ref")
			require.ErrorIs(t, err, ErrInvalidPath)

			// The branch is not poisoned: the next valid write commits AND syncs.
			_, err = svc.Facts().WriteFact(ctx, "main", "kb/architecture/ok/y.md",
				testFactBody("y", 0.9, nil), "learn: ok", "learn")
			require.NoError(t, err)
		})
	}
}

// %q makes a control character visible instead of breaking the line.
func TestWriteFact_InvalidPathErrorQuotesThePath(t *testing.T) {
	svc := newInvalidPathTestStore(t)
	_, err := svc.Facts().WriteFact(context.Background(), "main", "kb/architecture/a\nb/x.md", "body", "m", "learn")
	require.ErrorIs(t, err, ErrInvalidPath)
	require.Contains(t, err.Error(), `invalid path "kb/architecture/a\nb/x.md": `)
	require.NotContains(t, err.Error(), "a\nb", "the raw line feed must not reach the message")
}

func TestWriteFactIfUnchanged_RefusesUnreadablePath(t *testing.T) {
	svc := newInvalidPathTestStore(t)
	before := headOfBranch(t, svc, "main")
	_, err := svc.Facts().WriteFactIfUnchanged(context.Background(), "main",
		".knomit/jobs/a\nb/x.md", "v", "update", "update", "deadbeef")
	require.ErrorIs(t, err, ErrInvalidPath)
	require.Equal(t, before, headOfBranch(t, svc, "main"))
}

func TestBatchWriteFacts_RefusesUnreadablePath(t *testing.T) {
	for name, bad := range unreadablePaths {
		t.Run(name, func(t *testing.T) {
			svc := newInvalidPathTestStore(t)
			ctx := context.Background()
			before := headOfBranch(t, svc, "main")

			// One bad path among good ones refuses the whole batch.
			_, _, err := svc.Facts().BatchWriteFacts(ctx, "main", map[string]string{
				"kb/architecture/ok/a.md": testFactBody("a", 0.9, nil),
				bad:                       testFactBody("x", 0.9, nil),
			}, nil, "learn: m", "learn")
			require.Error(t, err)
			require.Equal(t, before, headOfBranch(t, svc, "main"), "a refused batch must not move the ref")
			require.ErrorIs(t, err, ErrInvalidPath)

			_, _, err = svc.Facts().BatchWriteFacts(ctx, "main", nil, []string{bad}, "retract: m", "retract")
			require.ErrorIs(t, err, ErrInvalidPath)
			require.Equal(t, before, headOfBranch(t, svc, "main"))

			_, err = svc.Facts().DeleteFact(ctx, "main", bad, "retract: m")
			require.ErrorIs(t, err, ErrInvalidPath)
			require.Equal(t, before, headOfBranch(t, svc, "main"))
		})
	}
}

// knomit's own dot paths and root files still write.
func TestWriteFact_KnomitDotPathsStillWrite(t *testing.T) {
	svc := newInvalidPathTestStore(t)
	ctx := context.Background()
	_, err := svc.Facts().WriteFact(ctx, "main", ".knomit/jobs/sweep/state.md", "v1", "m", "update")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "main", ".domains/ontology.yaml", "topics: {}\n", "m", "update")
	require.NoError(t, err)
	_, err = svc.Facts().WriteRootFile(ctx, "main", "README.md", "# hi", "m", "update")
	require.NoError(t, err)
}

// A name made only of backslashes is refused by the per-name gate alone: the
// whole-path rule splits on '\' as well as '/', so the name vanishes there,
// but the tree walker sees it as an entry name with no parts and refuses it.
// Before the per-name gate it moved the ref and poisoned the branch.
func TestWriteFact_RefusesBackslashOnlyName(t *testing.T) {
	const path = `kb/architecture/\/x.md`
	require.NoError(t, func() error { _, err := (&object.Tree{}).FindEntry(path); return ignoreNotFound(err) }(),
		"the whole-path gate alone accepts it; only the per-name gate refuses")

	svc := newInvalidPathTestStore(t)
	ctx := context.Background()
	before := headOfBranch(t, svc, "main")
	_, err := svc.Facts().WriteFact(ctx, "main", path, "body", "learn: m", "learn")
	require.ErrorIs(t, err, ErrInvalidPath)
	require.Equal(t, before, headOfBranch(t, svc, "main"), "a refused write must not move the ref")

	_, _, err = svc.Facts().BatchWriteFacts(ctx, "main", map[string]string{path: "body"}, nil, "learn: m", "learn")
	require.ErrorIs(t, err, ErrInvalidPath)
	require.Equal(t, before, headOfBranch(t, svc, "main"))
}

// Empty segments are out of scope for validatePath (#384) and pass it. They
// are refused anyway, before the ref moves, by deriveBeforeAdvance's tree
// decode ("malformed tree: empty filename"), so they cannot poison a branch.
// This pins that claim in gitReadablePath's comment; it is not ErrInvalidPath.
func TestWriteFact_EmptySegmentRefusedBeforeRefMoves(t *testing.T) {
	for _, path := range []string{"kb//x.md", "/kb/x.md", "kb/x/"} {
		t.Run(path, func(t *testing.T) {
			require.NoError(t, validatePath(path))
			svc := newInvalidPathTestStore(t)
			before := headOfBranch(t, svc, "main")
			_, err := svc.Facts().WriteFact(context.Background(), "main", path, "body", "m", "learn")
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrInvalidPath)
			require.Equal(t, before, headOfBranch(t, svc, "main"))
		})
	}
}

func ignoreNotFound(err error) error {
	if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return nil
	}
	return err
}
