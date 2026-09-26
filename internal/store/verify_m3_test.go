package store

import (
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

// memRepo builds commits from flat {path: content} maps in memory, so M3 can
// be tested on exact shapes without the store's write path.
type memRepo struct {
	t  *testing.T
	st storer.EncodedObjectStorer
	n  int
}

func newMemRepo(t *testing.T) *memRepo { return &memRepo{t: t, st: memory.NewStorage()} }

// repoOn builds commits directly into another object store (a Service's), the
// way the store tests "pretend a fetch happened".
func repoOn(t *testing.T, st storer.EncodedObjectStorer) *memRepo { return &memRepo{t: t, st: st} }

func (r *memRepo) blob(content string) plumbing.Hash {
	o := r.st.NewEncodedObject()
	o.SetType(plumbing.BlobObject)
	w, _ := o.Writer()
	_, _ = io.WriteString(w, content)
	_ = w.Close()
	h, err := r.st.SetEncodedObject(o)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

func (r *memRepo) tree(files map[string]string) plumbing.Hash {
	sub := map[string]map[string]string{}
	var entries []object.TreeEntry
	for p, content := range files {
		if dir, rest, ok := strings.Cut(p, "/"); ok {
			if sub[dir] == nil {
				sub[dir] = map[string]string{}
			}
			sub[dir][rest] = content
			continue
		}
		entries = append(entries, object.TreeEntry{Name: p, Mode: filemode.Regular, Hash: r.blob(content)})
	}
	for dir, fs := range sub {
		entries = append(entries, object.TreeEntry{Name: dir, Mode: filemode.Dir, Hash: r.tree(fs)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	o := r.st.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(o); err != nil {
		r.t.Fatal(err)
	}
	h, err := r.st.SetEncodedObject(o)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

func (r *memRepo) commit(files map[string]string, parents ...*object.Commit) *object.Commit {
	r.n++
	when := time.Unix(1790000000+int64(r.n), 0).UTC()
	c := &object.Commit{
		Author:    object.Signature{Name: "t", Email: "t@t", When: when},
		Committer: object.Signature{Name: "t", Email: "t@t", When: when},
		Message:   "c",
		TreeHash:  r.tree(files),
	}
	for _, p := range parents {
		c.ParentHashes = append(c.ParentHashes, p.Hash)
	}
	o := r.st.NewEncodedObject()
	if err := c.Encode(o); err != nil {
		r.t.Fatal(err)
	}
	h, err := r.st.SetEncodedObject(o)
	if err != nil {
		r.t.Fatal(err)
	}
	got, err := object.GetCommit(r.st, h)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

func with(m map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(out, kv[i])
		} else {
			out[kv[i]] = kv[i+1]
		}
	}
	return out
}

func mustM3(t *testing.T, merge *object.Commit) m3Verdict {
	t.Helper()
	v, err := checkM3(merge)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestM3 is proposal test 7: the accepted GitHub shape and every refused one.
func TestM3(t *testing.T) {
	r := newMemRepo(t)
	base := map[string]string{"kb/a.md": "a1", "kb/r.md": "retract me", "kb/u.md": "u1"}
	B := r.commit(base)
	// main: retracts r, updates u. side: adds s.
	mainFiles := with(base, "kb/r.md", "", "kb/u.md", "u2")
	M := r.commit(mainFiles, B)
	sideFiles := with(base, "kb/s.md", "s1")
	S := r.commit(sideFiles, B)
	clean := with(mainFiles, "kb/s.md", "s1")

	t.Run("clean GitHub-style merge is accepted", func(t *testing.T) {
		if v := mustM3(t, r.commit(clean, M, S)); !v.OK {
			t.Fatalf("clean merge refused: %s", v.Reason)
		}
	})
	t.Run("(a) resurrecting a retracted path is refused", func(t *testing.T) {
		v := mustM3(t, r.commit(with(clean, "kb/r.md", "retract me"), M, S))
		if v.OK || !strings.Contains(v.Reason, "kb/r.md") {
			t.Fatalf("resurrect: %+v", v)
		}
	})
	t.Run("(b) reverting an update is refused", func(t *testing.T) {
		v := mustM3(t, r.commit(with(clean, "kb/u.md", "u1"), M, S))
		if v.OK || !strings.Contains(v.Reason, "kb/u.md") {
			t.Fatalf("revert: %+v", v)
		}
	})
	t.Run("(c) -s ours (drop the side's changes) is refused", func(t *testing.T) {
		v := mustM3(t, r.commit(mainFiles, M, S))
		if v.OK || !strings.Contains(v.Reason, "kb/s.md") {
			t.Fatalf("-s ours: %+v", v)
		}
	})
	t.Run("(d) an injected file is refused", func(t *testing.T) {
		v := mustM3(t, r.commit(with(clean, "kb/evil.md", "x"), M, S))
		if v.OK || !strings.Contains(v.Reason, "kb/evil.md") {
			t.Fatalf("inject: %+v", v)
		}
	})
	t.Run("both sides changed one path needs a signature", func(t *testing.T) {
		S2 := r.commit(with(base, "kb/u.md", "u3"), B)
		v := mustM3(t, r.commit(with(mainFiles, "kb/u.md", "u2"), M, S2))
		if v.OK || !strings.Contains(v.Reason, "both sides changed") {
			t.Fatalf("both sides: %+v", v)
		}
	})
	t.Run("(e) criss-cross (two merge bases) needs a signature", func(t *testing.T) {
		X := r.commit(with(base, "kb/x.md", "x"), B)
		Y := r.commit(with(base, "kb/y.md", "y"), B)
		XY := r.commit(with(base, "kb/x.md", "x", "kb/y.md", "y"), X, Y)
		YX := r.commit(with(base, "kb/x.md", "x", "kb/y.md", "y"), Y, X)
		v := mustM3(t, r.commit(with(base, "kb/x.md", "x", "kb/y.md", "y"), XY, YX))
		if v.OK || !strings.Contains(v.Reason, "merge bases") {
			t.Fatalf("criss-cross: %+v", v)
		}
	})
	t.Run("(f) an octopus needs a signature", func(t *testing.T) {
		O := r.commit(with(base, "kb/o.md", "o"), B)
		v := mustM3(t, r.commit(with(clean, "kb/o.md", "o"), M, S, O))
		if v.OK || !strings.Contains(v.Reason, "3 parents") {
			t.Fatalf("octopus: %+v", v)
		}
	})
}
