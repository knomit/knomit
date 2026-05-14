// Category H — Multi-repo isolation. This category has a single test
// that asserts the Storyboard can run two completely independent repos
// side by side, each with its own bare remote, and that work on repo A
// leaves repo B's on-disk state unchanged at every layer: its local
// git refs, its SQLite fact index, and the bytes of its bare remote's
// object store. This is the baseline isolation guarantee the rest of
// the test suite quietly depends on.
package storytests

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/testenv"
)

// ── H1 ────────────────────────────────────────────────────────────────────

// TestMultiRepo_WritesAndRemotesIndependent is the only Category H test.
// Two repos (A and B), two bare remotes (originA and originB). Snapshot
// B's remote dir and B's agent-branch fact count. Write a fact to A and
// push. Re-snapshot B's remote dir and B's fact count: both must be
// identical to the before state, byte-for-byte on disk.
func TestMultiRepo_WritesAndRemotesIndependent(t *testing.T) {
	t.Log("H1: two repos with independent remotes; writing to A leaves B's local and remote byte-identical")
	sb := testenv.NewStoryboard(t)

	remoteA := sb.BareRemote("originA")
	remoteB := sb.BareRemote("originB")

	a := sb.Repo("a").Connect(remoteA)
	b := sb.Repo("b").Connect(remoteB)

	aAgent := a.Branch("agent/test")
	bAgent := b.Branch("agent/test")

	// Seed B's agent branch with a fact so "before" is non-trivial.
	bAgent.Write("kb/b-only.md", testenv.Fact("b-only"), "B seeds its own fact")

	// Snapshot B's remote dir on disk and B's fact count before any
	// activity on A.
	remoteBSnapshotBefore := hashDirTree(t, remoteB.Dir())
	bFactCountBefore := bAgent.FactCount()

	// Now do work on A: write a fact on its agent branch and push to
	// A's remote. Under the post-rework model agents push to
	// agent/<host>, never to main.
	aAgent.Write("kb/a-only.md", testenv.Fact("a-only"), "A writes its own fact")
	result := aAgent.Push()
	require.True(t, result.Pushed, "A's push must succeed")

	// B's remote directory must be byte-identical to the before-snapshot.
	remoteBSnapshotAfter := hashDirTree(t, remoteB.Dir())
	require.Equal(t, remoteBSnapshotBefore, remoteBSnapshotAfter,
		"B's bare remote must be byte-identical after A's push")

	// B's local agent branch must still see exactly its own fact.
	require.Equal(t, bFactCountBefore, bAgent.FactCount(),
		"B's fact count must not change from A's activity")
	bAgent.Head().Fact("kb/b-only.md").MustExist()
	bAgent.Head().Fact("kb/a-only.md").MustNotExist()

	// Neither repo should see the other's facts anywhere in its SQLite
	// state or git history — both Verify clean.
	a.MustVerify()
	b.MustVerify()
}

// hashDirTree walks dir recursively and returns a sha256 of the
// (relpath, size, content) tuples sorted by path. Used by H1 to assert
// a bare remote directory is byte-identical before and after an
// unrelated operation on a different repo/remote. Any change anywhere
// under dir — new file, modified bytes, deleted file — changes the hash.
func hashDirTree(t *testing.T, dir string) string {
	t.Helper()
	type entry struct {
		rel  string
		data []byte
	}
	var entries []entry
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, entry{rel: rel, data: data})
		return nil
	})
	if err != nil {
		t.Fatalf("hashDirTree(%s): %v", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.rel))
		h.Write([]byte{0})
		h.Write(e.data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
