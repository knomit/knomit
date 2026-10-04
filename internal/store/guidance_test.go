package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// commitGuidanceEntry commits, on top of branch's tip, an entry name with the
// given mode and content in .knomit/guidance/ (which must already exist), and
// returns the new tip. It is how a symlink or a submodule gets into a tree:
// knomit's own write doors only ever write regular files.
func commitGuidanceEntry(t *testing.T, svc *Service, branch, name string, mode filemode.FileMode, content string) plumbing.Hash {
	t.Helper()
	rh := svc.rh
	ref, err := rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	require.NoError(t, err)
	tip, err := rh.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	root, err := tip.Tree()
	require.NoError(t, err)
	knomit, err := root.Tree(".knomit")
	require.NoError(t, err)
	guid, err := knomit.Tree("guidance")
	require.NoError(t, err)

	blob, err := writeBlobToStore(rh.gits, []byte(content))
	require.NoError(t, err)
	gh, err := upsertEntry(rh.gits, guid, object.TreeEntry{Name: name, Mode: mode, Hash: blob})
	require.NoError(t, err)
	kh, err := upsertEntry(rh.gits, knomit, object.TreeEntry{Name: "guidance", Mode: filemode.Dir, Hash: gh})
	require.NoError(t, err)
	rootHash, err := upsertEntry(rh.gits, root, object.TreeEntry{Name: ".knomit", Mode: filemode.Dir, Hash: kh})
	require.NoError(t, err)

	c := &object.Commit{Author: tip.Author, Committer: tip.Committer, Message: "entry " + name,
		TreeHash: rootHash, ParentHashes: []plumbing.Hash{tip.Hash}}
	obj := rh.gits.NewEncodedObject()
	require.NoError(t, c.Encode(obj))
	h, err := rh.gits.SetEncodedObject(obj)
	require.NoError(t, err)
	require.NoError(t, rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)))
	return h
}

// GuidanceAt (F23) reads one guidance file of the commit it is given, and
// only a regular UTF-8 text file of at most 16 KiB under .knomit/guidance/.
func TestGuidanceAt_ReadsOnlyUsableFilesAtTheCommit(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	tr := svc.Triggers()

	putRaw(t, svc, "main", ".knomit/guidance/ok.md", "Write a Claim: line.\n")
	putRaw(t, svc, "main", ".knomit/guidance/exact.md", strings.Repeat("a", fact.MaxGuidanceBytes))
	putRaw(t, svc, "main", ".knomit/guidance/big.md", strings.Repeat("a", fact.MaxGuidanceBytes+1))
	putRaw(t, svc, "main", ".knomit/guidance/nul.md", "a\x00b")
	putRaw(t, svc, "main", ".knomit/guidance/latin1.md", "caf\xe9")
	putRaw(t, svc, "main", ".knomit/guidance/dir/inner.md", "inner")
	putRaw(t, svc, "main", ".knomit/ontology-copy.md", "outside")
	commitGuidanceEntry(t, svc, "main", "link.md", filemode.Symlink, "ok.md")
	tip := commitGuidanceEntry(t, svc, "main", "sub.md", filemode.Submodule, "")
	agent := putRaw(t, svc, "agent/a", ".knomit/guidance/agent-only.md", "AGENT")

	blob, data, err := tr.GuidanceAt(ctx, tip, ".knomit/guidance/ok.md")
	require.NoError(t, err)
	require.Equal(t, "Write a Claim: line.\n", string(data))
	require.NotEmpty(t, blob)

	_, data, err = tr.GuidanceAt(ctx, tip, ".knomit/guidance/exact.md")
	require.NoError(t, err, "exactly 16 KiB is allowed")
	require.Len(t, data, fact.MaxGuidanceBytes)

	_, _, err = tr.GuidanceAt(ctx, tip, ".knomit/guidance/missing.md")
	require.ErrorIs(t, err, ErrNoGuidanceAtCommit)
	_, _, err = tr.GuidanceAt(ctx, tip, ".knomit/guidance/agent-only.md")
	require.ErrorIs(t, err, ErrNoGuidanceAtCommit, "the agent branch's file is not in main's commit")
	_, data, err = tr.GuidanceAt(ctx, agent, ".knomit/guidance/agent-only.md")
	require.NoError(t, err, "the reader reads exactly the commit it is given")
	require.Equal(t, "AGENT", string(data))

	for file, why := range map[string]string{
		".knomit/guidance/big.md":              "over the",
		".knomit/guidance/nul.md":              "not UTF-8 text",
		".knomit/guidance/latin1.md":           "not UTF-8 text",
		".knomit/guidance/dir":                 "not a regular file",
		".knomit/guidance/link.md":             "not a regular file",
		".knomit/guidance/sub.md":              "not a regular file",
		".knomit/ontology-copy.md":             "not under",
		".knomit/guidance/../ontology-copy.md": "not under",
		".knomit/guidance/./ok.md":             "not under",
		"kb/x.md":                              "not under",
	} {
		_, data, err := tr.GuidanceAt(ctx, tip, file)
		require.Truef(t, errors.Is(err, ErrGuidanceUnusable), "%s: %v", file, err)
		require.Containsf(t, err.Error(), why, "%s", file)
		require.Nil(t, data)
	}
}
