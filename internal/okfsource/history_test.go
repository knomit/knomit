package okfsource

import (
	"sort"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/stretchr/testify/require"

	"knomit/internal/okf"
)

// writeTree writes files as blobs and folds the nested trees bottom-up,
// returning the root tree hash. Entries are sorted git-canonically (directory
// names compare as if suffixed with "/"), which go-git's tree reader depends on.
func writeTree(t *testing.T, st storer.EncodedObjectStorer, files map[string]string) plumbing.Hash {
	t.Helper()
	type node struct {
		files   map[string]plumbing.Hash
		subdirs map[string]bool
	}
	nodes := map[string]*node{}
	ensure := func(dir string) *node {
		if nodes[dir] == nil {
			nodes[dir] = &node{files: map[string]plumbing.Hash{}, subdirs: map[string]bool{}}
		}
		return nodes[dir]
	}
	ensure("")

	split := func(p string) (string, string) {
		for i := len(p) - 1; i >= 0; i-- {
			if p[i] == '/' {
				return p[:i], p[i+1:]
			}
		}
		return "", p
	}

	for p, content := range files {
		obj := st.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		w, err := obj.Writer()
		require.NoError(t, err)
		_, err = w.Write([]byte(content))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		bh, err := st.SetEncodedObject(obj)
		require.NoError(t, err)

		dir, base := split(p)
		ensure(dir).files[base] = bh
		for dir != "" {
			parent, self := split(dir)
			ensure(parent).subdirs[self] = true
			dir = parent
		}
	}

	depth := func(d string) int {
		if d == "" {
			return 0
		}
		n := 1
		for i := range len(d) {
			if d[i] == '/' {
				n++
			}
		}
		return n
	}
	dirs := make([]string, 0, len(nodes))
	for d := range nodes {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return depth(dirs[i]) > depth(dirs[j]) })

	treeHash := map[string]plumbing.Hash{}
	for _, d := range dirs {
		n := nodes[d]
		var entries []object.TreeEntry
		for base, bh := range n.files {
			entries = append(entries, object.TreeEntry{Name: base, Mode: filemode.Regular, Hash: bh})
		}
		for base := range n.subdirs {
			child := base
			if d != "" {
				child = d + "/" + base
			}
			entries = append(entries, object.TreeEntry{Name: base, Mode: filemode.Dir, Hash: treeHash[child]})
		}
		key := func(e object.TreeEntry) string {
			if e.Mode == filemode.Dir {
				return e.Name + "/"
			}
			return e.Name
		}
		sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })

		tree := &object.Tree{Entries: entries}
		obj := st.NewEncodedObject()
		require.NoError(t, tree.Encode(obj))
		th, err := st.SetEncodedObject(obj)
		require.NoError(t, err)
		treeHash[d] = th
	}
	return treeHash[""]
}

// commitWith authors a commit with an exact tree and exact parents. Tests use
// it instead of a worktree because a worktree cannot express a merge, and the
// merge cases are precisely what the history walk has to get right.
func commitWith(t *testing.T, r *git.Repository, msg, email string, when time.Time, files map[string]string, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	sig := object.Signature{Name: "t", Email: email, When: when}
	c := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      msg,
		TreeHash:     writeTree(t, r.Storer, files),
		ParentHashes: parents,
	}
	obj := r.Storer.NewEncodedObject()
	require.NoError(t, c.Encode(obj))
	h, err := r.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	return h
}

// TestLoad_RevisionsAreOldestFirst guards the walk-order reversal. The commit
// walk is a preorder from the tip, so revisions accumulate newest-first;
// renderHistory's contract requires oldest-first, because caller order is the
// only thing that resolves same-timestamp chronology. Remove the reversal in
// okfHistory and this test must fail.
func TestLoad_RevisionsAreOldestFirst(t *testing.T) {
	r := newFixtureRepo(t)
	const path = "kb/decisions/x/d9d6557d.md"
	c1 := commitWith(t, r, "learn: create", "a+learn@agents.knomit.io", baseTime,
		map[string]string{path: factBody("Scope", 0.5)})
	c2 := commitWith(t, r, "learn: revise", "a+learn@agents.knomit.io", baseTime.Add(time.Minute),
		map[string]string{path: factBody("Scope", 0.9)}, c1)

	snap, err := Load(r.Storer, c2)
	require.NoError(t, err)
	require.Len(t, snap.Facts, 1)

	revs := snap.Facts[0].Revisions
	require.Len(t, revs, 2, "both writes should be recorded")
	require.Equal(t, 0.5, revs[0].Confidence,
		"revisions must be oldest-first: the creating write (0.5) comes first")
	require.Equal(t, 0.9, revs[len(revs)-1].Confidence,
		"revisions must be oldest-first: the latest write (0.9) comes last")
	for _, rev := range revs {
		require.False(t, rev.Date.IsZero(), "revision date must come from the commit")
	}
}

// Authoring time is the EARLIEST commit that touched a path — the date the
// knowledge was first asserted, not the date it was last edited.
func TestHistory_AuthoringTimeIsEarliestCommit(t *testing.T) {
	r := newFixtureRepo(t)
	const path = "kb/decisions/x/d9d6557d.md"
	first := baseTime
	c1 := commitWith(t, r, "learn: create", "a+learn@agents.knomit.io", first,
		map[string]string{path: factBody("Scope", 0.5)})
	c2 := commitWith(t, r, "learn: revise", "a+learn@agents.knomit.io", first.AddDate(0, 1, 0),
		map[string]string{path: factBody("Scope", 0.9)}, c1)

	hist, err := okfHistory(r.Storer, c2)
	require.NoError(t, err)
	require.True(t, hist.Authored[path].Equal(first),
		"authoring time is the first touch, got %s want %s", hist.Authored[path], first)
}

// Exactly one Creation event per path: a path's Creation is decided by the diff
// (absent from the parent tree), not by timestamp equality, so commits sharing
// a wall-second must not both be labelled Creation.
func TestHistory_ExactlyOneCreationPerPath(t *testing.T) {
	r := newFixtureRepo(t)
	const path = "kb/decisions/x/d9d6557d.md"
	sameSecond := baseTime
	c1 := commitWith(t, r, "learn: create", "a+learn@agents.knomit.io", sameSecond,
		map[string]string{path: factBody("Scope", 0.5)})
	c2 := commitWith(t, r, "learn: revise", "a+learn@agents.knomit.io", sameSecond,
		map[string]string{path: factBody("Scope", 0.9)}, c1)

	hist, err := okfHistory(r.Storer, c2)
	require.NoError(t, err)

	var creations, updates int
	for _, e := range hist.Events {
		if e.Path != path {
			continue
		}
		switch e.Kind {
		case "Creation":
			creations++
		case "Update":
			updates++
		}
	}
	require.Equal(t, 1, creations, "exactly one Creation for the path")
	require.GreaterOrEqual(t, updates, 1, "at least one Update for the path")
}

func TestOKFOperationLabel(t *testing.T) {
	cases := map[string]string{
		"mindev.local-8ef0cd32+learn@agents.knomit.io":   "learn",
		"mindev.local-8ef0cd32+subsume@agents.knomit.io": "subsume",
		"k@knomit.io":                            "human", // a real person's commit identity
		"mindev.local-8ef0cd32@agents.knomit.io": "edit",
		"":                                       "edit", // absence of an agent address is NOT evidence of a person
	}
	for email, want := range cases {
		if got := okfOperationLabel(email); got != want {
			t.Errorf("okfOperationLabel(%q) = %q, want %q", email, got, want)
		}
	}
}

// okfParseRetirement reads the structural vocabulary knomit writes into its own
// retirement commits. It must never invent a successor: only an explicitly
// named kb/ path makes a retirement "superseded".
func TestOKFParseRetirement(t *testing.T) {
	cases := []struct {
		name     string
		message  string
		wantKind string
		wantSucc string
	}{
		{
			name:     "subsumed by a named fact is superseded",
			message:  "synthesize-review: subsumed by kb/technology/ai/models/xai/grok/grok-4-5.md",
			wantKind: okf.RetiredSuperseded,
			wantSucc: "kb/technology/ai/models/xai/grok/grok-4-5.md",
		},
		{
			name:     "explicit retract is retracted with no successor",
			message:  "manual-review: retract kb/meta/reasoning/b67c7b15.md",
			wantKind: okf.RetiredRetracted,
		},
		{
			name:     "anything else is retracted with no successor",
			message:  "Merge pull request #3 from knomit/agent/x\n\nAgent x",
			wantKind: okf.RetiredRetracted,
		},
		{
			// "subsumed by" without a kb/ path names no successor to LINK, but
			// the fact was still absorbed rather than withdrawn as wrong — so it
			// is superseded with an empty successor, not retracted.
			name:     "subsumed by unnamed text is superseded with no successor",
			message:  "synthesize-review: subsumed by distilled fact",
			wantKind: okf.RetiredSuperseded,
		},
		{
			name:     "dedup removal is retracted",
			message:  "dedup: remove duplicate kb/technology/ai/enterprise/ec65a4a2.md",
			wantKind: okf.RetiredRetracted,
		},
		{
			name:     "empty message is retracted",
			message:  "",
			wantKind: okf.RetiredRetracted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, succ := okfParseRetirement(tc.message)
			require.Equal(t, tc.wantKind, kind)
			require.Equal(t, tc.wantSucc, succ)
		})
	}
}

// TestHistory_MergeCarryingFileUnchangedIsNotARevision pins unchangedInAnotherParent.
// Diffing a merge against its FIRST parent alone reports every file the merge
// carried in from the other side as added or modified, even though nobody
// edited it — which manufactured revisions for untouched facts.
func TestHistory_MergeCarryingFileUnchangedIsNotARevision(t *testing.T) {
	r := newFixtureRepo(t)
	const (
		base   = "kb/decisions/x/aaaaaaaa.md"
		merged = "kb/decisions/x/bbbbbbbb.md"
		other  = "kb/decisions/x/cccccccc.md"
	)
	baseFact, mergedFact, otherFact := factBody("Base", 0.9), factBody("Merged", 0.9), factBody("Other", 0.9)

	b := commitWith(t, r, "learn: seed base", "a+learn@agents.knomit.io", baseTime,
		map[string]string{base: baseFact})
	// A feature branch writes `merged`; main advances independently.
	feature := commitWith(t, r, "learn: write on feature", "a+learn@agents.knomit.io", baseTime.Add(time.Minute),
		map[string]string{base: baseFact, merged: mergedFact}, b)
	main := commitWith(t, r, "learn: advance main", "a+learn@agents.knomit.io", baseTime.Add(2*time.Minute),
		map[string]string{base: baseFact, other: otherFact}, b)
	// The merge carries `merged` across unchanged. First parent is main.
	merge := commitWith(t, r, "Merge branch 'feature'", "a@agents.knomit.io", baseTime.Add(3*time.Minute),
		map[string]string{base: baseFact, merged: mergedFact, other: otherFact}, main, feature)

	hist, err := okfHistory(r.Storer, merge)
	require.NoError(t, err)

	require.Len(t, hist.Revisions[merged], 1,
		"a merge that carries a file in unchanged must not record a revision")
	require.Equal(t, "learn", hist.Revisions[merged][0].Operation)
}

// TestHistory_MergeCarryingDeletionPreservesClassification pins
// absentInAnotherParent. Attributing a branch's retirement to the merge commit
// would read the merge subject ("Merge branch …"), which carries none of
// knomit's structural vocabulary — silently downgrading every superseded fact
// reconciled through a merge to a bare "retracted".
func TestHistory_MergeCarryingDeletionPreservesClassification(t *testing.T) {
	r := newFixtureRepo(t)
	const (
		gone      = "kb/decisions/x/eeee1111.md"
		successor = "kb/decisions/x/d9d6557d.md"
		other     = "kb/decisions/x/cccccccc.md"
	)
	goneFact, succFact, otherFact := factBody("Gone", 0.7), factBody("Scope", 0.9), factBody("Other", 0.9)

	b := commitWith(t, r, "learn: seed", "a+learn@agents.knomit.io", baseTime,
		map[string]string{gone: goneFact, successor: succFact})
	// The branch retires `gone` with knomit's structural vocabulary.
	feature := commitWith(t, r, "synthesize-review: subsumed by "+successor, "a+subsume@agents.knomit.io", baseTime.Add(time.Minute),
		map[string]string{successor: succFact}, b)
	main := commitWith(t, r, "learn: advance main", "a+learn@agents.knomit.io", baseTime.Add(2*time.Minute),
		map[string]string{gone: goneFact, successor: succFact, other: otherFact}, b)
	merge := commitWith(t, r, "Merge branch 'feature'", "a@agents.knomit.io", baseTime.Add(3*time.Minute),
		map[string]string{successor: succFact, other: otherFact}, main, feature)

	hist, err := okfHistory(r.Storer, merge)
	require.NoError(t, err)

	require.Len(t, hist.Retired, 1)
	got := hist.Retired[0]
	require.Equal(t, gone, got.Path)
	require.Equal(t, "Gone", got.Title, "title comes from the blob the deletion removed")
	require.Equal(t, okf.RetiredSuperseded, got.Kind,
		"the branch commit's classification must survive the merge")
	require.Equal(t, successor, got.SuccessorPath)
}

// A fact deleted and then written again is LIVE, not retired. Only paths absent
// from the source commit's final tree may be reported as withdrawn — otherwise
// the index would disavow a claim the knowledge base still asserts.
func TestHistory_RecreatedFactIsNotRetired(t *testing.T) {
	r := newFixtureRepo(t)
	const (
		back  = "kb/decisions/x/ffff2222.md"
		stays = "kb/decisions/x/eeee1111.md"
	)
	c1 := commitWith(t, r, "learn: create", "a+learn@agents.knomit.io", baseTime,
		map[string]string{back: factBody("Back", 0.7), stays: factBody("Gone", 0.7)})
	c2 := commitWith(t, r, "manual-review: retract "+back, "k@knomit.io", baseTime.Add(time.Minute),
		map[string]string{stays: factBody("Gone", 0.7)}, c1)
	c3 := commitWith(t, r, "learn: recreate", "a+learn@agents.knomit.io", baseTime.Add(2*time.Minute),
		map[string]string{back: factBody("Back", 0.9), stays: factBody("Gone", 0.7)}, c2)
	c4 := commitWith(t, r, "manual-review: retract "+stays, "k@knomit.io", baseTime.Add(3*time.Minute),
		map[string]string{back: factBody("Back", 0.9)}, c3)

	hist, err := okfHistory(r.Storer, c4)
	require.NoError(t, err)

	var paths []string
	for _, rr := range hist.Retired {
		paths = append(paths, rr.Path)
	}
	require.NotContains(t, paths, back, "a recreated fact is live, not retired")
	require.Contains(t, paths, stays)
	require.Len(t, hist.Retired, 1)
}

// A deleted fact is collected as a retirement carrying its path and its title
// at the moment it was withdrawn — read from the pre-deletion blob, the only
// place that text still exists.
func TestHistory_CollectsRetirements(t *testing.T) {
	r := newFixtureRepo(t)
	const (
		gone      = "kb/decisions/x/eeee1111.md"
		successor = "kb/decisions/x/d9d6557d.md"
	)
	succFact := factBody("Scope", 0.9)
	c1 := commitWith(t, r, "learn: create", "a+learn@agents.knomit.io", baseTime,
		map[string]string{gone: factBody("Gone", 0.7), successor: succFact})
	c2 := commitWith(t, r, "synthesize-review: subsumed by "+successor, "a+subsume@agents.knomit.io", baseTime.Add(time.Minute),
		map[string]string{successor: succFact}, c1)

	hist, err := okfHistory(r.Storer, c2)
	require.NoError(t, err)

	require.Len(t, hist.Retired, 1)
	got := hist.Retired[0]
	require.Equal(t, gone, got.Path)
	require.Equal(t, "Gone", got.Title)
	require.Equal(t, okf.RetiredSuperseded, got.Kind)
	require.Equal(t, successor, got.SuccessorPath)
}
