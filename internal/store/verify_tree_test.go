package store

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/store/internal/vcs"
)

// The malformed-tree rows: a directory entry (mode 040000) naming a blob, and
// one naming an object that does not exist. go-git v5.19.2's TreeWalker ends
// the walk silently at either (Next turns the failed GetTree into io.EOF), so
// a rule built on tree.Files(), NewTreeWalker or DiffTree never sees the
// entry or anything after it.
const missingHash = "1111111111111111111111111111111111111111"

func bogusDirs(r *memRepo) []struct {
	name string
	hash plumbing.Hash
} {
	return []struct {
		name string
		hash plumbing.Hash
	}{
		{"blob as dir", r.blob("payload")},
		{"missing object", plumbing.NewHash(missingHash)},
	}
}

// encodeTree writes es (sorted by name) as a tree into st.
func encodeTree(t *testing.T, st storer.EncodedObjectStorer, es []object.TreeEntry) plumbing.Hash {
	t.Helper()
	es = append([]object.TreeEntry{}, es...)
	sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
	o := st.NewEncodedObject()
	require.NoError(t, (&object.Tree{Entries: es}).Encode(o))
	h, err := st.SetEncodedObject(o)
	require.NoError(t, err)
	return h
}

// graftTree is the tree root with extra entries added to the (existing)
// subtree at dir ("" = the root itself); every tree on the way is rewritten.
func graftTree(t *testing.T, st storer.EncodedObjectStorer, root plumbing.Hash, dir string, extra ...object.TreeEntry) plumbing.Hash {
	t.Helper()
	tr, err := object.GetTree(st, root)
	require.NoError(t, err)
	es := append([]object.TreeEntry{}, tr.Entries...)
	if dir == "" {
		return encodeTree(t, st, append(es, extra...))
	}
	head, rest, _ := strings.Cut(dir, "/")
	found := 0
	for i := range es {
		if es[i].Name == head {
			es[i].Hash = graftTree(t, st, es[i].Hash, rest, extra...)
			found++
		}
	}
	require.Equal(t, 1, found, "graft: %s is in the tree", head)
	return encodeTree(t, st, es)
}

// shapeCommit writes a commit with the given tree and parents, authored like
// rangeFixture.commit (agent "" = a plain address), signed when s is set.
func shapeCommit(t *testing.T, st storer.EncodedObjectStorer, tree plumbing.Hash, agent string, s ssh.Signer, parents ...*object.Commit) *object.Commit {
	t.Helper()
	email := "t@t"
	if agent != "" {
		email = agent + "+learn@agents.knomit.io"
	}
	when := parents[0].Committer.When.Add(3600e9).UTC()
	c := &object.Commit{Author: object.Signature{Name: agent, Email: email, When: when},
		Committer: object.Signature{Name: agent, Email: email, When: when}, Message: "m", TreeHash: tree}
	for _, p := range parents {
		c.ParentHashes = append(c.ParentHashes, p.Hash)
	}
	if s != nil {
		c = signed(t, c, s)
	}
	o := st.NewEncodedObject()
	require.NoError(t, c.Encode(o))
	h, err := st.SetEncodedObject(o)
	require.NoError(t, err)
	got, err := object.GetCommit(st, h)
	require.NoError(t, err)
	return got
}

// bogusMergeOf is repro 1's commit: an UNSIGNED merge of tip and anc whose
// tree is tip's plus, under kb/, a Dir entry "zz-bogus" naming bogus and then
// a real file "zzz-evil.md" that sorts after it.
func bogusMergeOf(t *testing.T, st storer.EncodedObjectStorer, tip, anc *object.Commit, bogus plumbing.Hash) *object.Commit {
	t.Helper()
	r := repoOn(t, st)
	tree := graftTree(t, st, tip.TreeHash, "kb",
		object.TreeEntry{Name: "zz-bogus", Mode: filemode.Dir, Hash: bogus},
		object.TreeEntry{Name: "zzz-evil.md", Mode: filemode.Regular, Hash: r.blob("injected")})
	return shapeCommit(t, st, tree, "", nil, tip, anc)
}

// CheckTreeShape refuses each malformed shape and names the entry, nested
// ones included; a well-formed tree with a gitlink passes. Row count asserted.
//
// SABOTAGE: make CheckTreeShape return nil → every refused row passes → red.
func TestCheckTreeShape(t *testing.T) {
	r := newMemRepo(t)
	good := r.tree(map[string]string{"kb/a.md": "a", "kb/notes/b.md": "b"})
	sub := r.tree(map[string]string{"x.md": "x"})
	rows := []struct {
		name, dir string
		entry     object.TreeEntry
		want      string // "" = well formed; else a substring of the error
	}{
		{"blob as dir", "kb", object.TreeEntry{Name: "zz", Mode: filemode.Dir, Hash: r.blob("p")}, "kb/zz (mode 0040000"},
		{"missing dir", "kb", object.TreeEntry{Name: "zz", Mode: filemode.Dir, Hash: plumbing.NewHash(missingHash)}, "kb/zz (mode 0040000"},
		{"nested blob as dir", "kb/notes", object.TreeEntry{Name: "deep", Mode: filemode.Dir, Hash: r.blob("p")}, "kb/notes/deep (mode"},
		{"tree as file", "kb", object.TreeEntry{Name: "f.md", Mode: filemode.Regular, Hash: sub}, "kb/f.md (mode 0100644"},
		{"missing file", "kb", object.TreeEntry{Name: "f.md", Mode: filemode.Regular, Hash: plumbing.NewHash(missingHash)}, "is not a blob"},
		{"missing symlink", "kb", object.TreeEntry{Name: "l", Mode: filemode.Symlink, Hash: plumbing.NewHash(missingHash)}, "kb/l (mode 0120000"},
		{"duplicate name", "kb", object.TreeEntry{Name: "a.md", Mode: filemode.Regular, Hash: r.blob("again")}, "appears twice"},
		{"a name go-git will not read", "kb", object.TreeEntry{Name: ".git", Mode: filemode.Dir, Hash: sub}, "kb/.git (mode"},
		{"a valid subtree", "kb", object.TreeEntry{Name: "zz", Mode: filemode.Dir, Hash: sub}, ""},
		{"a gitlink to a commit not here", "kb", object.TreeEntry{Name: "mod", Mode: filemode.Submodule, Hash: plumbing.NewHash(missingHash)}, ""},
	}
	ran := 0
	for _, row := range rows {
		th := graftTree(t, r.st, good, row.dir, row.entry)
		tree, err := object.GetTree(r.st, th)
		require.NoError(t, err)
		err = vcs.CheckTreeShape(tree, map[plumbing.Hash]bool{})
		if row.want == "" {
			require.NoError(t, err, row.name)
		} else {
			require.Error(t, err, row.name)
			require.Contains(t, err.Error(), row.want, row.name)
		}
		ran++
	}
	require.Equal(t, len(rows), ran)

	// Depth: a chain of MaxShapeDepth+2 trees fails, one of MaxShapeDepth
	// passes (go-git's own walker stops at the same depth).
	chain := func(n int) *object.Tree {
		h := r.tree(map[string]string{"x.md": "x"})
		for range n {
			h = encodeTree(t, r.st, []object.TreeEntry{{Name: "d", Mode: filemode.Dir, Hash: h}})
		}
		tree, err := object.GetTree(r.st, h)
		require.NoError(t, err)
		return tree
	}
	require.NoError(t, vcs.CheckTreeShape(chain(vcs.MaxShapeDepth), nil))
	err := vcs.CheckTreeShape(chain(vcs.MaxShapeDepth+2), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "deeper than")
}

// M3 fails on a malformed entry instead of ending the walk there: the
// reviewer's scratch repro (an otherwise clean merge plus "kc" naming a blob
// or a missing object, then "zz-evil.md") was {OK:true}.
//
// SABOTAGE: drop the CheckTreeShape call in checkM3's flat → OK → red.
func TestM3_AMalformedTreeIsAnError(t *testing.T) {
	r := newMemRepo(t)
	base := map[string]string{"kb/a.md": "a1", "kb/u.md": "u1"}
	B := r.commit(base)
	mainFiles := with(base, "kb/u.md", "u2")
	M := r.commit(mainFiles, B)
	S := r.commit(with(base, "kb/s.md", "s1"), B)
	clean := r.tree(with(mainFiles, "kb/s.md", "s1"))
	ran := 0
	for _, b := range bogusDirs(r) {
		th := graftTree(t, r.st, clean, "",
			object.TreeEntry{Name: "kc", Mode: filemode.Dir, Hash: b.hash},
			object.TreeEntry{Name: "zz-evil.md", Mode: filemode.Regular, Hash: r.blob("injected content")})
		mc := shapeCommit(t, r.st, th, "", nil, M, S)
		v, err := checkM3(mc)
		require.Error(t, err, "%s: %+v", b.name, v)
		require.False(t, v.OK, b.name)
		require.Contains(t, err.Error(), "M3: merge tree: kc (mode", b.name)
		ran++
	}
	require.Equal(t, 2, ran)
}

// Repro 1, CheckRange: an unsigned merge on a plain enforce-mode knowledge
// base that smuggles kb/zzz-evil.md in behind a bogus directory entry was
// admitted (checked=1 refused=[]). It is now refused malformed-tree, naming
// the entry, for a blob-as-dir and a missing object; and the same on the
// fleet repository judging itself.
//
// SABOTAGE: make CheckTreeShape return nil → both rows admitted → red.
func TestCheckRange_MalformedTreeRefused(t *testing.T) {
	ran := 0
	for _, kind := range []string{"blob as dir", "missing object"} {
		f := newRangeFixture(t)
		boss := namedSigner(t, "boss")
		f.setFleet(map[string]string{"boss/1.md": memberFile("boss", fact.MemberActive, boss.PublicKey())})
		base := map[string]string{fact.OntologyFile: kbOntology("enforce"), "kb/notes/a.md": "a"}
		root := f.commit(f.kb, base, "", nil)
		m2 := f.commit(f.kb, with(base, "kb/notes/b.md", "b"), "boss", boss, root)
		f.ref(f.kb, "main", m2)
		b := bogusDirs(repoOn(t, f.kb.Storer))[ran]
		require.Equal(t, kind, b.name)
		mc := bogusMergeOf(t, f.kb.Storer, m2, root, b.hash)
		f.ref(f.kb, "cand", mc)
		v, err := f.check("enforce")
		require.NoError(t, err)
		require.Equal(t, 1, v.Checked, kind)
		got := onlyRefusal(t, v)
		require.Equal(t, RuleMalformedTree, got.Rule, kind)
		require.Equal(t, mc.Hash.String(), got.Commit, kind)
		require.Contains(t, got.Reason, "kb/zz-bogus", kind)
		ran++
	}
	require.Equal(t, 2, ran)

	f, main, base, boss, _ := ownerFixture(t)
	main2 := f.commit(f.fleet, with(base, "kb/members/boss/1.md", base["kb/members/boss/1.md"]+"\nv2\n"), "boss", boss, main)
	f.ref(f.fleet, "main", main2)
	mc := bogusMergeOf(t, f.fleet.Storer, main2, main, repoOn(t, f.fleet.Storer).blob("payload"))
	f.ref(f.fleet, "cand", mc)
	got := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, RuleMalformedTree, got.Rule, "fleet repository")
	require.Contains(t, got.Reason, "kb/zz-bogus")
}

// bogusOwnFolderCommit is repro 2's commit: agent-x's SIGNED single-parent
// commit that only adds kb/members/agent-x/zz, a Dir entry naming a blob.
func bogusOwnFolderCommit(t *testing.T, f *rangeFixture, main *object.Commit, x ssh.Signer) *object.Commit {
	t.Helper()
	st := f.fleet.Storer
	tree := graftTree(t, st, main.TreeHash, "kb/members/agent-x",
		object.TreeEntry{Name: "zz", Mode: filemode.Dir, Hash: repoOn(t, st).blob("p")})
	return shapeCommit(t, st, tree, "agent-x", x, main)
}

// Repro 2: a member's signed commit adding a bogus directory inside its own
// folder was admitted, and once on main the loader stopped at it, so boss
// (whose folder sorts after agent-x's) vanished and became no-record. Now
// the commit is refused malformed-tree, and a main that already holds such a
// tree (admitted before this check) makes the loader FAIL, never return
// fewer members: CheckRange cannot run (an error) instead of judging boss
// no-record.
//
// SABOTAGE: make CheckTreeShape return nil → the commit is admitted, the
// loader returns 1 member and boss's commit is no-record → red.
func TestCheckRange_MalformedTreeInTheOwnFolder(t *testing.T) {
	f, main, base, boss, x := ownerFixture(t)
	evil := bogusOwnFolderCommit(t, f, main, x)
	f.ref(f.fleet, "cand", evil)
	got := onlyRefusal(t, f.checkSelf(""))
	require.Equal(t, RuleMalformedTree, got.Rule)
	require.Equal(t, evil.Hash.String(), got.Commit)
	require.Contains(t, got.Reason, "kb/members/agent-x/zz")

	// Control: before the bogus commit, the loader sees both members.
	ms, err := loadMembers("t", main, "kb")
	require.NoError(t, err)
	require.Len(t, ms, 2)

	// The commit reached main anyway (the gate was not in its way).
	ms, err = loadMembers("t", evil, "kb")
	require.Error(t, err, "loaded %d members", len(ms))
	require.Contains(t, err.Error(), "kb/members/agent-x/zz")
	f.ref(f.fleet, "main", evil)
	next := f.commit(f.fleet, with(base, "kb/members/boss/1.md", base["kb/members/boss/1.md"]+"\nv2\n"), "boss", boss, evil)
	f.ref(f.fleet, "cand", next)
	_, err = CheckRange(RangeInput{KBDir: f.fleetDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
	require.Error(t, err, "the gate cannot run on a fleet whose tree is malformed")
	require.Contains(t, err.Error(), "kb/members/agent-x/zz")
}

// pushNew sends c to h as branch with a hand-built pack holding exactly the
// peer's objects the host does not hold: the pack a real attacker sends. (A
// go-git client walks the malformed tree with the same truncating walker and
// leaves objects out.)
func pushNew(t *testing.T, h *pushHost, pe *peer, c *object.Commit, branch string) string {
	t.Helper()
	var hs []plumbing.Hash
	it, err := pe.repo.Storer.IterEncodedObjects(plumbing.AnyObject)
	require.NoError(t, err)
	require.NoError(t, it.ForEach(func(o plumbing.EncodedObject) error {
		if h.svc.rh.Storer().HasEncodedObject(o.Hash()) != nil {
			hs = append(hs, o.Hash())
		}
		return nil
	}))
	var pack bytes.Buffer
	_, err = packfile.NewEncoder(&pack, pe.repo.Storer, false).Encode(hs, 10)
	require.NoError(t, err)
	return rawPushPack(t, h.srv.URL, pack.Bytes(), &packp.Command{Name: plumbing.NewBranchReferenceName(branch), New: c.Hash})
}

// The receive-pack rows. The host's completeness walk (reachableNew) refuses
// a directory entry whose object the push SENT and is not a tree, and an
// object neither sent nor held; but it trusts every object the host already
// holds, "and therefore everything under it". So a directory entry naming a
// blob the HOST holds passed it, and the private-path closure (DiffTree
// against the merge base) and the judge were silent on it. Each such push is
// now refused malformed-tree and its branch is not created: under enforce on
// the fleet repository (repro 1, repro 2), and under verify_signatures off on
// a plain knowledge base, where the closure is the only check and a bogus
// entry sorting before .knomit/ hid a change to the ontology from it. The
// missing-object row is the completeness walk's (unchanged) refusal.
//
// SABOTAGE: make CheckTreeShape return nil → the three malformed-tree rows
// are admitted → red.
func TestReceivePack_MalformedTreeRefused(t *testing.T) {
	p := newTestFleetPKI(t)
	r, boss := namedSigner(t, "rogue"), namedSigner(t, "boss")
	rl, _ := p.leaf("rogue", r)
	bl, _ := p.leaf("boss", boss)
	extra := p.artifacts(p.crl(), map[string]string{"rogue": rl, "boss": bl})
	extra["kb/members/rogue/1.md"] = memberFile("rogue", fact.MemberActive, r.PublicKey())
	fleetHost := newFleetPushHost(t, extra)
	fp, _ := pusherOf(t, r)
	fleetHost.setPolicy(PushPolicy{Pusher: fp, OwnBranch: hostBranch, Fleet: func(context.Context) (Fleet, error) {
		return fleetHost.svc.FleetAt("main")
	}})
	offHost := newPushHost(t, "off", 0)

	// held is the hash of a blob the host holds: the file at p on main.
	held := func(pe *peer, head *object.Commit, p string) plumbing.Hash {
		content, ok := pe.files(head)[p]
		require.True(t, ok, p)
		return repoOn(t, pe.repo.Storer).blob(content)
	}
	type row struct {
		name string
		host *pushHost
		make func(pe *peer, head *object.Commit) *object.Commit
		want []string
	}
	rows := []row{
		{"unsigned merge, a dir naming a held blob", fleetHost, func(pe *peer, head *object.Commit) *object.Commit {
			side := pe.commitAs("rogue", []*object.Commit{head}, r, with(pe.files(head), "kb/members/rogue/2.md", "a"))
			return bogusMergeOf(t, pe.repo.Storer, side, head, held(pe, head, "kb/members/boss/1.md"))
		}, []string{RuleMalformedTree, "kb/zz-bogus"}},
		{"signed commit, a dir naming a held blob in the own folder", fleetHost, func(pe *peer, head *object.Commit) *object.Commit {
			st := pe.repo.Storer
			tree := graftTree(t, st, head.TreeHash, "kb/members/rogue",
				object.TreeEntry{Name: "zz", Mode: filemode.Dir, Hash: held(pe, head, "kb/members/boss/1.md")})
			return shapeCommit(t, st, tree, "rogue", r, head)
		}, []string{RuleMalformedTree, "kb/members/rogue/zz"}},
		{"verify off, a dir naming a held blob before kb/.hidden.md", offHost, func(pe *peer, head *object.Commit) *object.Commit {
			st := pe.repo.Storer
			bogus := held(pe, head, fact.OntologyFile)
			c := pe.commitAs("peer-abcd1234", []*object.Commit{head}, nil,
				with(pe.files(head), "kb/.hidden.md", "a private path"))
			tree := graftTree(t, st, c.TreeHash, "kb", object.TreeEntry{Name: "-", Mode: filemode.Dir, Hash: bogus})
			return shapeCommit(t, st, tree, "", nil, head)
		}, []string{RuleMalformedTree, "kb/- (mode"}},
		{"a dir naming a missing object", fleetHost, func(pe *peer, head *object.Commit) *object.Commit {
			side := pe.commitAs("rogue", []*object.Commit{head}, r, with(pe.files(head), "kb/members/rogue/3.md", "b"))
			return bogusMergeOf(t, pe.repo.Storer, side, head, plumbing.NewHash(missingHash))
		}, []string{"the push is incomplete", missingHash}},
	}
	ran := 0
	for i, tc := range rows {
		pe := newPeer(t, tc.host.srv.URL)
		head := pe.head()
		c := tc.make(pe, head)
		branch := "agent/bogus-" + string(rune('a'+i))
		out := pushNew(t, tc.host, pe, c, branch)
		for _, w := range tc.want {
			require.Contains(t, out, w, tc.name)
		}
		require.Equal(t, plumbing.ZeroHash, tc.host.ref(branch), "%s: a refused branch is not created", tc.name)
		ran++
	}
	require.Equal(t, 4, ran)
}

// A judged commit's PARENT is often not judged itself: main's tip below a
// CheckRange range, or the host-held upstream tip under a push. When that
// parent's tree is malformed, the rules that diff parent → c (recordOwner,
// artifactRules, the closure's DiffTree) read a truncated parent: a delete
// of a path behind the bogus entry is invisible and a rewrite reads as an
// insert. So commitTreeShape checks every parent's tree too, and a
// well-formed commit on a malformed parent is refused malformed-tree, the
// reason naming the parent.
//
// malformedTip commits on base the tree of base plus, under kb/, a Dir entry
// "zz-bogus" naming a blob and then kb/zzz-victim.md, which sorts after it.
func malformedTip(t *testing.T, st storer.EncodedObjectStorer, base *object.Commit, agent string, s ssh.Signer) *object.Commit {
	t.Helper()
	r := repoOn(t, st)
	tree := graftTree(t, st, base.TreeHash, "kb",
		object.TreeEntry{Name: "zz-bogus", Mode: filemode.Dir, Hash: r.blob("payload")},
		object.TreeEntry{Name: "zzz-victim.md", Mode: filemode.Regular, Hash: r.blob("victim v1")})
	return shapeCommit(t, st, tree, agent, s, base)
}

// requireWellFormed asserts c's OWN tree passes CheckTreeShape, so a refusal
// can only come from a parent's.
func requireWellFormed(t *testing.T, c *object.Commit) {
	t.Helper()
	tree, err := c.Tree()
	require.NoError(t, err)
	require.NoError(t, vcs.CheckTreeShape(tree, nil), "the judged commit's own tree is well formed")
}

// CheckRange: main's tip P, below the range, holds the malformed tree. boss's
// well-formed signed single-parent commit on P drops the bogus entry and
// rewrites kb/zzz-victim.md. It is the one commit judged, and it is refused
// malformed-tree, the reason naming the parent P and the entry.
//
// SABOTAGE: loop commitTreeShape over c.ParentHashes[:0] → the commit is
// admitted → red.
func TestCheckRange_MalformedParentTreeRefused(t *testing.T) {
	f := newRangeFixture(t)
	boss := namedSigner(t, "boss")
	f.setFleet(map[string]string{"boss/1.md": memberFile("boss", fact.MemberActive, boss.PublicKey())})
	base := map[string]string{fact.OntologyFile: kbOntology("enforce"), "kb/notes/a.md": "a"}
	root := f.commit(f.kb, base, "", nil)
	st := f.kb.Storer
	p := malformedTip(t, st, root, "boss", boss)
	f.ref(f.kb, "main", p)
	c := shapeCommit(t, st, repoOn(t, st).tree(with(base, "kb/zzz-victim.md", "victim v2")), "boss", boss, p)
	requireWellFormed(t, c)
	require.Equal(t, []plumbing.Hash{p.Hash}, c.ParentHashes)
	f.ref(f.kb, "cand", c)

	v, err := f.check("enforce")
	require.NoError(t, err)
	require.Equal(t, 1, v.Checked)
	got := onlyRefusal(t, v)
	require.Equal(t, RuleMalformedTree, got.Rule)
	require.Equal(t, c.Hash.String(), got.Commit)
	require.Contains(t, got.Reason, "parent "+p.Hash.String()+": kb/zz-bogus (mode")
}

// receive-pack: the host's main (the upstream tip) holds the malformed tree,
// all its objects host-held; the peer pushes one well-formed commit on it
// that rewrites kb/zzz-victim.md. Under verify_signatures off, where the
// closure is the only check, the push is refused malformed-tree naming the
// parent, the branch is not created and main does not move.
//
// SABOTAGE: loop commitTreeShape over c.ParentHashes[:0] → the push is
// admitted → red.
func TestReceivePack_MalformedUpstreamTipRefused(t *testing.T) {
	h := newPushHost(t, VerifyOff, 1)
	pe := newPeer(t, h.srv.URL)
	p := malformedTip(t, pe.repo.Storer, pe.head(), "", nil)
	// Plant P as the host's main, every object of it host-held.
	it, err := pe.repo.Storer.IterEncodedObjects(plumbing.AnyObject)
	require.NoError(t, err)
	copied := 0
	require.NoError(t, it.ForEach(func(o plumbing.EncodedObject) error {
		if h.svc.rh.Storer().HasEncodedObject(o.Hash()) == nil {
			return nil
		}
		copied++
		_, err := h.svc.rh.Storer().SetEncodedObject(o)
		return err
	}))
	require.Positive(t, copied)
	require.NoError(t, h.svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), p.Hash)))

	c := pe.commitAs("peer-abcd1234", []*object.Commit{p}, nil,
		with(pe.files(pe.head()), "kb/zzz-victim.md", "victim v2"))
	requireWellFormed(t, c)
	const branch = "agent/bogus-up"
	out := pushNew(t, h, pe, c, branch)
	require.Contains(t, out, "knomit: commit "+c.Hash.String()+" refused: "+RuleMalformedTree)
	require.Contains(t, out, "parent "+p.Hash.String()+": kb/zz-bogus (mode")
	require.Equal(t, plumbing.ZeroHash, h.ref(branch), "a refused branch is not created")
	require.Equal(t, p.Hash, h.ref("main"), "main does not move")
}
