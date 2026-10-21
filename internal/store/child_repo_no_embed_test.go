package store

import (
	"reflect"
	"testing"

	"knomit/internal/store/facts"
	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/merge"
	"knomit/internal/store/remotesync"
)

// TestChildRepos_DoNotEmbedVcsRepo pins E2 of the store refactor: facts.Repo,
// merge.Repo and remotesync.Repo hold the git layer in a NAMED unexported
// field, never as an embedded *vcs.Repo. An embedded one promotes every
// exported vcs.Repo method (LockBranch, NotifyCommit, CommitSigs, Storer,
// SetRefForTest, ...) onto the child, and any caller handed the child could
// then move a ref or write an object around the write path's lock order.
//
// A by-value embed (vcs.Repo) promotes the same methods as the pointer embed
// and is flagged too. The walk follows embedded struct fields, so a child that embeds another
// child which embeds *vcs.Repo (remotesync.Repo embeds *merge.Repo) is caught
// too. It also requires that every type holds a vcs.Repo field somewhere, so
// the test cannot pass because the handle was removed.
func TestChildRepos_DoNotEmbedVcsRepo(t *testing.T) {
	vcsRepo := reflect.TypeOf((*vcs.Repo)(nil))
	vcsRepoVal := vcsRepo.Elem() // a by-value embed promotes the same methods
	for _, c := range []struct {
		name string
		typ  reflect.Type
	}{
		{"facts.Repo", reflect.TypeOf(facts.Repo{})},
		{"merge.Repo", reflect.TypeOf(merge.Repo{})},
		{"remotesync.Repo", reflect.TypeOf(remotesync.Repo{})},
	} {
		holdsHandle := false
		var walk func(path string, st reflect.Type)
		walk = func(path string, st reflect.Type) {
			for i := 0; i < st.NumField(); i++ {
				f := st.Field(i)
				if f.Type == vcsRepo || f.Type == vcsRepoVal {
					holdsHandle = true
					if f.Anonymous {
						t.Errorf("%s embeds vcs.Repo or *vcs.Repo (via %s.%s): every exported vcs.Repo method is promoted onto it", c.name, path, f.Name)
					}
				}
				if f.Anonymous {
					ft := f.Type
					if ft.Kind() == reflect.Ptr {
						ft = ft.Elem()
					}
					if ft.Kind() == reflect.Struct {
						walk(path+"."+f.Name, ft)
					}
				}
			}
		}
		walk(c.name, c.typ)
		if !holdsHandle {
			t.Errorf("%s holds no *vcs.Repo field at all: the test is not looking at the handle", c.name)
		}
	}
}
