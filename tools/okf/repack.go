package main

import (
	"errors"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// The two recoveries report themselves differently because they cost the reader
// differently: one repeats a screenful of stages that needs accounting for, the
// other repeats nothing visible. Both are printed only once the rescan has
// actually recovered the run, so each reports what happened rather than
// guessing at a cause.
const (
	repackedRerunNote  = "the repository was repacked mid-run (git maintenance); the stages above ran twice"
	repackedRescanNote = "the repository was repacked mid-run (git maintenance); rescanned the object store"
)

// retryAfterRepack runs op, and runs it once more if it failed because git
// repacked the repository underneath us.
//
// A knomit-okf run holds one repository open for seconds — a clone of a large
// base for tens of them — while git is entitled to reorganise the object store
// at any moment. `git maintenance run --auto` fires after ordinary commands
// like commit and detaches into the background, writing loose objects into a
// new packfile and then deleting them. go-git builds its packfile index ONCE,
// on first use (storage/filesystem.ObjectStorage.requireIndex), and never
// rescans: an object that moved into a pack created after that point is missing
// from the cached index and gone from the loose directory, so it reads as
// absent. The run then dies on a repository `git fsck` calls intact — and the
// user's own `git commit` is what started the repack.
//
// Rescanning is the remedy go-git documents for this ("useful if git changed
// packfiles externally"). One rescan is enough, and that is why this retries
// exactly once: git writes the new pack before it prunes the loose copy, so
// every object is readable in one place or the other at every instant. A second
// failure is therefore not a repack, and looping on it would turn a clear
// failure into a hang.
//
// The recovery is announced only when it WORKED. The two errors below are
// ambiguous — a tree that is genuinely absent reports itself the same way as
// one that moved — so a note printed on the way into the retry would be a guess,
// and on the honest reading of a stale-cache miss the retry is what proves it.
// The price of that ambiguity is one wasted attempt on a real failure, which is
// the cheaper side of the trade: an export costs seconds, a run that dies with
// an unactionable error costs the user their afternoon.
//
// op must be safe to run twice. Both callers are: rendering is a pure function
// of the source commit, reconcile and staging converge on the same tree, and a
// commit that never happened cannot be duplicated.
func retryAfterRepack(repo *git.Repository, onRecovered func(), op func() error) error {
	err := op()
	if !isMissingObject(err) {
		return err
	}
	// Only the on-disk storage caches a packfile index; an in-memory one cannot
	// go stale, and reporting the original error is right for anything else.
	reindexer, ok := repo.Storer.(interface{ Reindex() })
	if !ok {
		return err
	}
	reindexer.Reindex()
	err = op()
	if isMissingObject(err) {
		// The rescan found nothing new, so the fault is real. Report the second
		// attempt's error: it is the one that survived a fresh view of the store.
		return err
	}
	// Recovery is "the object became readable", NOT "the operation succeeded".
	// The two part company: a retried commit legitimately ends in ErrEmptyCommit,
	// which is how this tool says "nothing changed" — keying the announcement on
	// a nil error made every recovery on that path silent, so the run repeated
	// work it never accounted for.
	if onRecovered != nil {
		onRecovered()
	}
	return err
}

// isMissingObject reports whether err is go-git failing to find an object —
// including the case where it has already lost the reason. Tree.Tree turns
// ErrObjectNotFound from a subtree read into ErrDirectoryNotFound, so a
// repacked tree and a path that was never there arrive as the same error, and
// this had to be widened after a run failed with "directory not found" instead.
func isMissingObject(err error) bool {
	return errors.Is(err, plumbing.ErrObjectNotFound) ||
		errors.Is(err, object.ErrDirectoryNotFound)
}
