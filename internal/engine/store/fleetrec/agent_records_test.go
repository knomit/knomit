package fleetrec

import (
	"crypto/ed25519"
	"sort"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/domain/fact"
)

// memRepo builds raw trees and commits in a memory store, so a test can make
// shapes no write path produces (a blob where a folder belongs, a missing
// subtree object).
type memRepo struct {
	t  *testing.T
	st *memory.Storage
}

func (r memRepo) blob(content []byte) plumbing.Hash {
	o := r.st.NewEncodedObject()
	o.SetType(plumbing.BlobObject)
	w, err := o.Writer()
	require.NoError(r.t, err)
	_, err = w.Write(content)
	require.NoError(r.t, err)
	require.NoError(r.t, w.Close())
	h, err := r.st.SetEncodedObject(o)
	require.NoError(r.t, err)
	return h
}

func (r memRepo) tree(entries ...object.TreeEntry) plumbing.Hash {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	tr := &object.Tree{Entries: entries}
	o := r.st.NewEncodedObject()
	require.NoError(r.t, tr.Encode(o))
	h, err := r.st.SetEncodedObject(o)
	require.NoError(r.t, err)
	return h
}

// branch points branch at a new root commit of tree.
func (r memRepo) branch(name string, tree plumbing.Hash) {
	sig := object.Signature{Name: "t", Email: "t@t", When: time.Unix(1, 0)}
	c := &object.Commit{Author: sig, Committer: sig, Message: "m", TreeHash: tree}
	o := r.st.NewEncodedObject()
	require.NoError(r.t, c.Encode(o))
	h, err := r.st.SetEncodedObject(o)
	require.NoError(r.t, err)
	require.NoError(r.t, r.st.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), h)))
}

func dirEntry(name string, h plumbing.Hash) object.TreeEntry {
	return object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h}
}

func fileEntry(name string, h plumbing.Hash) object.TreeEntry {
	return object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h}
}

func leftRecord(t *testing.T, agent, serial string) []byte {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	k, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return []byte("---\nkind: pragmatic\ntype: policy\n---\n# member " + agent + "\n\n" +
		fact.RenderMember(fact.Member{Agent: agent, State: fact.MemberLeft, Key: k, Host: "h", LeafSerial: serial}))
}

// kbWith is the root tree kb/members/<folders>.
func (r memRepo) kbWith(folders ...object.TreeEntry) plumbing.Hash {
	return r.tree(dirEntry("kb", r.tree(dirEntry("members", r.tree(folders...)))))
}

// AgentRecordsAt (#651 review B1, S1) reads ONE agent's folder: its own
// records only, none when the folder is absent, and an ERROR (never "none")
// when the folder is there but cannot be read, so a caller that fails closed
// on an error really does.
// SABOTAGE: read the folder with tree.Tree (a missing subtree object reads as
// ErrDirectoryNotFound) and map that to "none" -> the missing-object case
// returns no error -> red.
func TestAgentRecordsAt_OwnFolderOnlyAndUnreadableIsAnError(t *testing.T) {
	r := memRepo{t: t, st: memory.NewStorage()}
	alphaLeft := r.blob(leftRecord(t, "alpha", "a1"))
	// mallory's folder holds a left record that names alpha's serial, and
	// (skipped by the loader) a record FOR alpha planted in mallory's folder.
	malloryOwn := r.blob(leftRecord(t, "mallory", "a1"))
	planted := r.blob(leftRecord(t, "alpha", "a1"))

	r.branch("agent/ok", r.kbWith(
		dirEntry("alpha", r.tree(fileEntry("r.md", alphaLeft))),
		dirEntry("mallory", r.tree(fileEntry("r.md", malloryOwn), fileEntry("x.md", planted))),
	))
	r.branch("agent/mallory-only", r.kbWith(
		dirEntry("mallory", r.tree(fileEntry("r.md", malloryOwn), fileEntry("x.md", planted))),
	))
	r.branch("agent/blob-folder", r.kbWith(fileEntry("alpha", alphaLeft)))
	r.branch("agent/missing-folder", r.kbWith(dirEntry("alpha", plumbing.NewHash("1111111111111111111111111111111111111111"))))

	recs, err := AgentRecordsAt(r.st, "agent/ok", "kb", "alpha")
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, "alpha", recs[0].Agent)
	require.Equal(t, fact.MemberLeft, recs[0].State)
	require.Equal(t, "a1", recs[0].LeafSerial)
	require.Equal(t, "kb/members/alpha/r.md", recs[0].Path)

	recs, err = AgentRecordsAt(r.st, "agent/mallory-only", "kb", "alpha")
	require.NoError(t, err)
	require.Empty(t, recs, "another member's folder, and a record planted there for alpha, are not alpha's")

	recs, err = AgentRecordsAt(r.st, "agent/ok", "kb", "mallory")
	require.NoError(t, err)
	require.Len(t, recs, 1, "the planted record is skipped; mallory's own counts")
	require.Equal(t, "mallory", recs[0].Agent)

	_, err = AgentRecordsAt(r.st, "agent/blob-folder", "kb", "alpha")
	require.Error(t, err, "a blob where alpha's folder belongs")

	_, err = AgentRecordsAt(r.st, "agent/missing-folder", "kb", "alpha")
	require.Error(t, err, "a folder whose tree object is missing is unreadable, not empty")

	recs, err = AgentRecordsAt(r.st, "agent/missing-folder", "kb", "bob")
	require.NoError(t, err)
	require.Empty(t, recs, "bob has no folder: none, and alpha's broken folder is not bob's problem")
}
