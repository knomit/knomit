package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// TestRetryAfterRepack_RecoversFromAConcurrentRepack reproduces the race a long
// run loses against git's own maintenance, and pins the recovery.
//
// git runs `git maintenance run --auto` after ordinary commands like commit,
// detached into the background. It packs loose objects and then deletes them.
// go-git builds its packfile index ONCE, on first use
// (storage/filesystem.ObjectStorage.requireIndex) and never rescans, so an
// object that moves from loose into a pack created after that point is in
// neither place this process will look: the loose file is gone and the new pack
// is not in the cached index. Every later lookup fails with a bare "object not
// found" on a repository git itself reports as intact.
func TestRetryAfterRepack_RecoversFromAConcurrentRepack(t *testing.T) {
	dir := t.TempDir()
	writer, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	h := writeLooseBlob(t, writer, "a fact worth publishing")

	// The handle a sync holds for the length of its run. Asking for a hash that
	// is not there populates the packfile-index cache without caching the object
	// under test — the exact state the race needs, and the state any real run
	// reaches as soon as it reads one object out of a pack.
	held, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	absent := plumbing.NewHash("1111111111111111111111111111111111111111")
	if _, err := held.Storer.EncodedObject(plumbing.BlobObject, absent); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("priming lookup: want ErrObjectNotFound, got %v", err)
	}

	repackBehindItsBack(t, dir, h)

	// The defect: on disk, reachable by git, unreadable by this handle.
	if _, err := held.Storer.EncodedObject(plumbing.BlobObject, h); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("this test must reproduce the stale-cache miss first; got %v", err)
	}

	// The recovery. The object is never in neither place at once — git writes
	// the pack before it prunes the loose copy — so one rescan is enough.
	recovered := 0
	err = retryAfterRepack(held, func() { recovered++ }, func() error {
		_, e := held.Storer.EncodedObject(plumbing.BlobObject, h)
		return e
	})
	if err != nil {
		t.Fatalf("retryAfterRepack: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("want exactly one announced recovery, got %d", recovered)
	}
}

// TestRetryAfterRepack_CoversTheMaskedTreeError pins the second face of the same
// race. Tree.Tree turns ErrObjectNotFound from a subtree read into
// ErrDirectoryNotFound, so a repacked tree arrives under a name that says
// nothing about objects — and a recovery keyed only on ErrObjectNotFound walks
// straight past it. Found by a real run that failed with "directory not found"
// after the first version of this fix had already handled the other face.
func TestRetryAfterRepack_CoversTheMaskedTreeError(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = retryAfterRepack(repo, func() {}, func() error {
		calls++
		if calls == 1 {
			return fmt.Errorf("read kb: %w", object.ErrDirectoryNotFound)
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("want a retry after a wrapped ErrDirectoryNotFound, got %v after %d calls", err, calls)
	}
}

// TestRetryAfterRepack_AnnouncesARecoveryThatEndsInASentinel pins the split
// between "the object became readable" and "the operation succeeded". A retried
// commit legitimately ends in ErrEmptyCommit — this tool's way of saying nothing
// changed — and that is the most common outcome of a re-sync. Keying the
// announcement on a nil error made every recovery on that path silent, so a run
// repeated work and said nothing; caught by a live run whose recovery left no
// trace in its own output.
func TestRetryAfterRepack_AnnouncesARecoveryThatEndsInASentinel(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	announced := 0
	calls := 0
	err = retryAfterRepack(repo, func() { announced++ }, func() error {
		calls++
		if calls == 1 {
			return plumbing.ErrObjectNotFound
		}
		return git.ErrEmptyCommit
	})
	if !errors.Is(err, git.ErrEmptyCommit) {
		t.Fatalf("the sentinel must reach the caller unchanged, got %v", err)
	}
	if announced != 1 {
		t.Fatalf("want the recovery announced once, got %d", announced)
	}
}

// TestRetryAfterRepack_DoesNotRetryOtherFailures keeps the recovery narrow: a
// genuinely missing object is a different fault from a repacked one, and
// running an expensive export twice to reach the same error helps nobody.
func TestRetryAfterRepack_DoesNotRetryOtherFailures(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("bundle is not conformant")
	calls := 0
	err = retryAfterRepack(repo, func() { t.Fatal("must not announce a recovery") }, func() error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("want the original error after 1 call, got %v after %d", err, calls)
	}
}

// TestRetryAfterRepack_GivesUpAfterOneRescan bounds the recovery. A rescan that
// does not resolve the object means something other than a repack is wrong, and
// looping on it would turn a clear failure into a hang.
func TestRetryAfterRepack_GivesUpAfterOneRescan(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = retryAfterRepack(repo, func() { t.Fatal("must not claim a recovery that did not happen") }, func() error {
		calls++
		return plumbing.ErrObjectNotFound
	})
	if !errors.Is(err, plumbing.ErrObjectNotFound) || calls != 2 {
		t.Fatalf("want ErrObjectNotFound after 2 calls, got %v after %d", err, calls)
	}
}

// writeLooseBlob stores a blob the way go-git stores everything it writes: as a
// loose object, which is what makes it a candidate for the next repack.
func writeLooseBlob(t *testing.T, repo *git.Repository, content string) plumbing.Hash {
	t.Helper()
	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// repackBehindItsBack does what `git maintenance` does to a repository someone
// else is holding open: writes the objects into a new packfile through a
// SEPARATE handle, then removes the loose copies. Separate on purpose — writing
// through the held handle would update its own index and hide the defect.
func repackBehindItsBack(t *testing.T, dir string, hashes ...plumbing.Hash) {
	t.Helper()
	other, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	pw, ok := other.Storer.(storer.PackfileWriter)
	if !ok {
		t.Fatal("storer cannot write packfiles")
	}
	w, err := pw.PackfileWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := packfile.NewEncoder(w, other.Storer, false).Encode(hashes, 10); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, h := range hashes {
		s := h.String()
		if err := os.Remove(filepath.Join(dir, ".git", "objects", s[:2], s[2:])); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRetryAfterRepack_SurvivesAWrappingThatBreaksTheChain pins the one case
// errors.Is cannot reach. go-git's tree diff wraps with
// `fmt.Errorf("from: %s", err)` — %s, not %w (utils/merkletrie/doubleiter.go) —
// which destroys the error chain outright. Every wt.Status goes through that
// path, so `sync`'s own up-to-date check produced a repack failure no amount of
// sentinel matching could catch: found by a live run that failed with
// "from: directory not found" after the typed matching was already in.
func TestRetryAfterRepack_SurvivesAWrappingThatBreaksTheChain(t *testing.T) {
	broken := fmt.Errorf("from: %s", object.ErrDirectoryNotFound)
	if errors.Is(broken, object.ErrDirectoryNotFound) {
		t.Fatal("this test premise is that a percent-s wrap breaks the chain; it no longer does")
	}
	if !isMissingObject(broken) {
		t.Fatal("a chain-breaking wrap of a missing-object error must still be recognised")
	}

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = retryAfterRepack(repo, func() {}, func() error {
		calls++
		if calls == 1 {
			return fmt.Errorf("from: %s", plumbing.ErrObjectNotFound)
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("want a retry after a chain-breaking wrap, got %v after %d calls", err, calls)
	}
}

// TestIsMissingObject_StaysNarrow keeps the substring fallback from swallowing
// unrelated failures into a pointless second attempt.
func TestIsMissingObject_StaysNarrow(t *testing.T) {
	for _, e := range []error{
		nil,
		errors.New("generated bundle is not conformant: kb/x.md: empty or missing type"),
		errors.New("authentication required"),
		errors.New("worktree contains unstaged changes"),
	} {
		if isMissingObject(e) {
			t.Fatalf("must not treat %v as a repack symptom", e)
		}
	}
}
