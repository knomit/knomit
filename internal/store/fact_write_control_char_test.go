package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// #384: a path holding a control character is one go-git refuses to READ
// (ValidTreePath, applied by FindEntry, the tree walker and DiffTree), but
// nothing stopped it being WRITTEN. The read that refuses it runs in
// notifyCommit's im.Sync, after the ref has moved, so the write committed,
// reported failure, and left a commit every later sync on the branch tripped
// over. Every write door must refuse the path before the ref moves.

func newControlCharTestStore(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc
}

func headOfBranch(t *testing.T, svc *Service, branch string) string {
	t.Helper()
	h, err := svc.Branches().HeadCommit(context.Background(), branch)
	require.NoError(t, err)
	return h
}

// controlCharPaths covers a line feed (the reported case), a tab, NUL, and
// DEL (0x7f), the one control character above 0x20.
var controlCharPaths = map[string]string{
	"line feed": "kb/architecture/a\nb/x.md",
	"tab":       "kb/architecture/a\tb/x.md",
	"nul":       "kb/architecture/a\x00b/x.md",
	"del":       "kb/architecture/a\x7fb/x.md",
}

func TestWriteFact_RefusesControlCharacterPath(t *testing.T) {
	for name, path := range controlCharPaths {
		t.Run(name, func(t *testing.T) {
			svc := newControlCharTestStore(t)
			ctx := context.Background()
			before := headOfBranch(t, svc, "main")

			_, err := svc.Facts().WriteFact(ctx, "main", path, "body", "learn: m", "learn")
			require.Error(t, err)
			require.Equal(t, before, headOfBranch(t, svc, "main"), "a refused write must not move the ref")
			require.Contains(t, err.Error(), "contains a control character")
			// %q makes the character visible instead of breaking the line.
			require.NotContains(t, err.Error(), path)

			// The branch is not poisoned: the next valid write commits AND syncs.
			_, err = svc.Facts().WriteFact(ctx, "main", "kb/architecture/ok/y.md",
				testFactBody("y", 0.9, nil), "learn: ok", "learn")
			require.NoError(t, err)
		})
	}
}

func TestWriteFactIfUnchanged_RefusesControlCharacterPath(t *testing.T) {
	svc := newControlCharTestStore(t)
	before := headOfBranch(t, svc, "main")
	_, err := svc.Facts().WriteFactIfUnchanged(context.Background(), "main",
		".knomit/jobs/a\nb/x.md", "v", "update", "update", "deadbeef")
	require.ErrorContains(t, err, "contains a control character")
	require.Equal(t, before, headOfBranch(t, svc, "main"))
}

func TestBatchWriteFacts_RefusesControlCharacterPath(t *testing.T) {
	svc := newControlCharTestStore(t)
	ctx := context.Background()
	before := headOfBranch(t, svc, "main")

	// One bad path among good ones refuses the whole batch.
	_, _, err := svc.Facts().BatchWriteFacts(ctx, "main", map[string]string{
		"kb/architecture/ok/a.md":   testFactBody("a", 0.9, nil),
		"kb/architecture/a\nb/x.md": testFactBody("x", 0.9, nil),
	}, nil, "learn: m", "learn")
	require.Error(t, err)
	require.Equal(t, before, headOfBranch(t, svc, "main"), "a refused batch must not move the ref")
	require.ErrorContains(t, err, `invalid path "kb/architecture/a\nb/x.md": contains a control character`)
	require.ErrorIs(t, err, ErrInvalidPath, "callers map it to a 400 with errors.Is")

	_, _, err = svc.Facts().BatchWriteFacts(ctx, "main", nil,
		[]string{"kb/architecture/a\nb/x.md"}, "retract: m", "retract")
	require.ErrorContains(t, err, "contains a control character")
	require.Equal(t, before, headOfBranch(t, svc, "main"))

	_, err = svc.Facts().DeleteFact(ctx, "main", "kb/architecture/a\nb/x.md", "retract: m")
	require.ErrorContains(t, err, "contains a control character")
	require.Equal(t, before, headOfBranch(t, svc, "main"))
}

// The exists check answers in the writers' words, not go-git's.
func TestFactExists_ControlCharacterPathNamesTheRule(t *testing.T) {
	svc := newControlCharTestStore(t)
	_, err := svc.Facts().FactExists(context.Background(), "main", ".knomit/jobs/a\nb/x.md")
	require.EqualError(t, err, `invalid path ".knomit/jobs/a\nb/x.md": contains a control character`)
}
