package store

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// validatePath returns an error if path is empty or contains "..".
// It does not normalise case; callers must lower-case before calling.
func validatePath(path string) error {
	if path == "" {
		return fmt.Errorf("path must not be empty")
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("path must not contain '..'")
	}
	return nil
}

// writeFile writes content to path in a new commit with message on branch,
// normalizing case first. Returns the commit hash and the blob hash.
//
// The lowercasing keeps FACT paths free of case-duplicate topics ("AI" vs
// "ai") — see fact.NormalizePath. It is a fact-path rule, not a storage rule,
// so writeFileExact is the door for files that are not facts.
func (fi *factIndex) writeFile(ctx context.Context, branch, path, content, message, operation string) (commitHash string, blobHash string, err error) {
	return fi.writeFileExact(ctx, branch, strings.ToLower(path), content, message, operation, "")
}

// writeFileExact is writeFile without case normalization. Callers own the
// exact bytes of the path they pass.
//
// A non-empty expectBlob is a compare-and-swap precondition: the blob at path
// on the branch tip must be expectBlob, checked INSIDE the branch write lock so
// no other write can land between the check and the commit. On a mismatch
// nothing is written and the error wraps ErrFactChanged.
func (fi *factIndex) writeFileExact(ctx context.Context, branch, path, content, message, operation, expectBlob string) (commitHash string, blobHash string, err error) {
	if fi.rh.readOnly {
		return "", "", ErrRepoReadOnly
	}
	if err := validatePath(path); err != nil {
		return "", "", fmt.Errorf("store: WriteFile: %w", err)
	}

	unlock := fi.rh.lockBranch(branch)
	defer unlock()

	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return "", "", fmt.Errorf("WriteFile: ref: %w", err)
	}
	if expectBlob != "" {
		current, err := fi.rh.blobAtCommit(headHash, path)
		if err != nil {
			return "", "", fmt.Errorf("WriteFile: precondition: %w", err)
		}
		if current != expectBlob {
			return "", "", fmt.Errorf("WriteFile: %q: %w", path, ErrFactChanged)
		}
	}

	signer, err := fi.rh.commitSigner()
	if err != nil {
		return "", "", fmt.Errorf("WriteFile: %w", err)
	}
	author := fi.rh.authorSig(branch, operation)
	committer := fi.rh.committerSig(branch)
	// The causal-trace trailers (F07): stamped here, on the message the
	// builder signs, when the caller's ctx carries a set; nothing otherwise.
	message = appendTrailers(message, trailersFromContext(ctx))
	newCommitHash, newBlobHash, err := writeFileToStore(fi.rh.gits, signer, headHash, path, content, message, author, committer)
	if err != nil {
		return "", "", err
	}

	// Update the branch ref to point to the new commit.
	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := fi.rh.gits.SetReference(plumbing.NewHashReference(branchRefName, newCommitHash)); err != nil {
		return "", "", err
	}

	// Notify inside the lock — the ref advance, commit_log append, and
	// im.Sync (which updates branch_facts / graph) must be atomic w.r.t.
	// concurrent readers (including Verify, which takes lockBranchRead).
	// A reader observing the window after SetReference but before
	// notifyCommit would see a torn state: git HEAD at the new commit,
	// SQL index still at the previous one.
	if err := fi.rh.notifyCommit(ctx, branch, newCommitHash); err != nil {
		return "", "", err
	}
	return newCommitHash.String(), newBlobHash.String(), nil
}

// deleteFile removes path from branch and creates a commit.
// Returns the commit hash of the new commit.
func (fi *factIndex) deleteFile(ctx context.Context, branch, path, message, operation string) (commitHash string, err error) {
	if fi.rh.readOnly {
		return "", ErrRepoReadOnly
	}
	path = strings.ToLower(path)
	if err := validatePath(path); err != nil {
		return "", fmt.Errorf("store: DeleteFile: %w", err)
	}

	unlock := fi.rh.lockBranch(branch)
	defer unlock()

	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("DeleteFile: ref: %w", err)
	}

	// Check existence inside the lock to avoid a TOCTOU race.
	exists, err := fi.fileExists(ctx, branch, path)
	if err != nil {
		return "", fmt.Errorf("DeleteFile: check exists: %w", err)
	}
	if !exists {
		return "", fmt.Errorf("DeleteFile: file %q does not exist", path)
	}

	signer, err := fi.rh.commitSigner()
	if err != nil {
		return "", fmt.Errorf("DeleteFile: %w", err)
	}
	author := fi.rh.authorSig(branch, operation)
	committer := fi.rh.committerSig(branch)
	message = appendTrailers(message, trailersFromContext(ctx)) // see writeFileExact
	newCommitHash, err := deleteFileFromStore(fi.rh.gits, signer, headHash, path, message, author, committer)
	if err != nil {
		return "", err
	}

	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := fi.rh.gits.SetReference(plumbing.NewHashReference(branchRefName, newCommitHash)); err != nil {
		return "", err
	}

	// notifyCommit runs inside the branch lock — see writeFile for rationale.
	if err := fi.rh.notifyCommit(ctx, branch, newCommitHash); err != nil {
		return "", err
	}
	return newCommitHash.String(), nil
}

// RetractMissingError is returned by BatchWriteFactsMustExist when one or
// more paths the caller requires to exist are absent at the branch tip. The
// check runs INSIDE the branch write lock, against the head the commit would
// be parented on, so nothing can land between it and the commit: on this
// error nothing was written. Paths lists every absent path, lowercased as the
// store keys them.
type RetractMissingError struct {
	Paths []string
}

func (e *RetractMissingError) Error() string {
	return "not on this branch: " + strings.Join(e.Paths, ", ")
}

// batchWriteUnlockedHook, when set, runs in batchWrite after the pre-flight
// checks and BEFORE the branch lock is taken. Tests only: it holds the window
// in which an existence check done outside the lock would go stale open, so a
// test of the under-the-lock precondition fails reliably when the check is
// moved out of the lock (F08 T-A3).
var batchWriteUnlockedHook atomic.Pointer[func()]

// SetBatchWriteUnlockedHookForTest installs f as the pre-lock hook of every
// batch write in this process and returns the function that removes it.
// EXISTS ONLY for tests.
func SetBatchWriteUnlockedHookForTest(f func()) (restore func()) {
	batchWriteUnlockedHook.Store(&f)
	return func() { batchWriteUnlockedHook.Store(nil) }
}

// batchWrite writes and deletes multiple files in one commit on branch.
// Returns the commit hash and a map of path → blob hash for each written file.
//
// Deletions are applied after the writes, so a path that appears in both ends
// up deleted. Callers relying on write-then-delete of the same path are almost
// certainly confused; keep the two sets disjoint.
//
// mustExist is a precondition (F04, the atomic move): every path in it must be
// present at the branch tip, checked UNDER the branch lock before any object
// is written; otherwise nothing is written and the error is a
// *RetractMissingError. nil means no precondition.
func (fi *factIndex) batchWrite(ctx context.Context, branch string, files map[string]string, deletes, mustExist []string, message, operation string) (commitHash string, blobHashes map[string]string, err error) {
	if fi.rh.readOnly {
		return "", nil, ErrRepoReadOnly
	}
	if len(files) == 0 && len(deletes) == 0 {
		return "", nil, nil
	}
	loweredMust := make([]string, len(mustExist))
	for i, path := range mustExist {
		loweredMust[i] = strings.ToLower(path)
	}
	mustExist = loweredMust

	// Lowercase all paths.
	lowered := make(map[string]string, len(files))
	for path, content := range files {
		lowered[strings.ToLower(path)] = content
	}
	files = lowered

	loweredDeletes := make([]string, len(deletes))
	for i, path := range deletes {
		loweredDeletes[i] = strings.ToLower(path)
	}
	deletes = loweredDeletes

	// Pre-flight validation: reject empty paths and paths containing "..".
	for path := range files {
		if err := validatePath(path); err != nil {
			return "", nil, fmt.Errorf("store: batchWrite: %w", err)
		}
	}
	for _, path := range deletes {
		if err := validatePath(path); err != nil {
			return "", nil, fmt.Errorf("store: batchWrite delete: %w", err)
		}
	}

	if h := batchWriteUnlockedHook.Load(); h != nil {
		(*h)()
	}

	unlock := fi.rh.lockBranch(branch)
	defer unlock()
	cHash, blobHashes, err := fi.batchWriteLocked(ctx, branch, files, deletes, mustExist, message, operation)
	if err != nil {
		return "", nil, err
	}

	// notifyCommit runs inside the branch lock — see writeFile for rationale.
	if err := fi.rh.notifyCommit(ctx, branch, cHash); err != nil {
		return "", nil, err
	}
	return cHash.String(), blobHashes, nil
}

// batchWriteLocked performs the actual batchWrite work. Caller must hold the branch lock.
func (fi *factIndex) batchWriteLocked(ctx context.Context, branch string, files map[string]string, deletes, mustExist []string, message, operation string) (plumbing.Hash, map[string]string, error) {
	// Before any object is written: a refused batch must leave nothing behind.
	signer, err := fi.rh.commitSigner()
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: %w", err)
	}
	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: ref: %w", err)
	}

	// The move's precondition (F04): checked here, under the lock, against
	// the head this commit is parented on. A delete of an absent LEAF would
	// otherwise succeed silently (removeEntry filters by name), so two local
	// takes of one task would both commit; checked here, the second sees the
	// first's commit and is refused.
	if len(mustExist) > 0 {
		var missing []string
		for _, path := range mustExist {
			blob := ""
			if headHash != plumbing.ZeroHash {
				if blob, err = fi.rh.blobAtCommit(headHash, path); err != nil {
					return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: precondition: %w", err)
				}
			}
			if blob == "" {
				missing = append(missing, path)
			}
		}
		if len(missing) > 0 {
			return plumbing.ZeroHash, nil, &RetractMissingError{Paths: missing}
		}
	}

	parentHash := headHash

	// Read existing root tree.
	var rootTree *object.Tree
	if parentHash != plumbing.ZeroHash {
		parentCommit, err := object.GetCommit(fi.rh.gits, parentHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: get parent commit: %w", err)
		}
		rootTree, err = parentCommit.Tree()
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: get parent tree: %w", err)
		}
	}

	blobHashes := make(map[string]string, len(files))

	// Apply each file to the tree sequentially. Seed the running root with the
	// parent's tree so a delete-only batch (no writes to advance it) still
	// commits the parent's content minus the deletions.
	var currentRootHash plumbing.Hash
	if rootTree != nil {
		currentRootHash = rootTree.Hash
	}
	for path, content := range files {
		// Create blob.
		blobObj := fi.rh.gits.NewEncodedObject()
		blobObj.SetType(plumbing.BlobObject)
		bw, err := blobObj.Writer()
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: blob writer for %q: %w", path, err)
		}
		if _, err := io.WriteString(bw, content); err != nil {
			bw.Close()
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: blob write for %q: %w", path, err)
		}
		bw.Close()
		blobHash, err := fi.rh.gits.SetEncodedObject(blobObj)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: store blob for %q: %w", path, err)
		}
		blobHashes[path] = blobHash.String()

		// Update tree.
		currentRootHash, err = buildTree(fi.rh.gits, rootTree, path, blobHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: build tree for %q: %w", path, err)
		}

		// Load updated root tree for next iteration.
		rootTree, err = object.GetTree(fi.rh.gits, currentRootHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: get updated tree: %w", err)
		}
	}

	// Apply deletions to the same running tree, so writes and retractions land
	// in one commit.
	for _, path := range deletes {
		if rootTree == nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: delete %q: branch has no tree", path)
		}
		currentRootHash, err = deleteFromTree(fi.rh.gits, rootTree, path)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: delete %q: %w", path, err)
		}
		rootTree, err = object.GetTree(fi.rh.gits, currentRootHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: get tree after delete: %w", err)
		}
	}

	// Create single commit.
	author := fi.rh.authorSig(branch, operation)
	committer := fi.rh.committerSig(branch)
	commit := &object.Commit{
		Author:    author,
		Committer: committer,
		Message:   appendTrailers(message, trailersFromContext(ctx)), // see writeFileExact
		TreeHash:  currentRootHash,
	}
	if parentHash != plumbing.ZeroHash {
		commit.ParentHashes = []plumbing.Hash{parentHash}
	}

	cHash, err := storeCommit(fi.rh.gits, signer, commit)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: %w", err)
	}

	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := fi.rh.gits.SetReference(plumbing.NewHashReference(branchRefName, cHash)); err != nil {
		return plumbing.ZeroHash, nil, err
	}
	return cHash, blobHashes, nil
}

// WriteFact writes a fact to the store and returns the commit and blob hashes.
// Index sync (branch_facts / facts_vec / graph) is triggered inside writeFile
// via rh.notifyCommit — no redundant fi.im.Sync call here.
func (fi *factIndex) WriteFact(ctx context.Context, branch, path, content, message, operation string) (WriteFactResult, error) {
	commitHash, blobHash, err := fi.writeFile(ctx, branch, path, content, message, operation)
	if err != nil {
		return WriteFactResult{}, err
	}
	return WriteFactResult{CommitHash: commitHash, BlobHash: blobHash}, nil
}

// WriteFactIfUnchanged is WriteFact with a compare-and-swap precondition: it
// commits only if path's blob on the branch tip is still expectBlob (the
// BlobHash a ReadFact{WithHash} returned), checked inside the branch write
// lock. Otherwise nothing is written and the error wraps ErrFactChanged. This
// is what makes a read-modify-write of one fact safe against a concurrent
// writer.
func (fi *factIndex) WriteFactIfUnchanged(ctx context.Context, branch, path, content, message, operation, expectBlob string) (WriteFactResult, error) {
	if expectBlob == "" {
		return WriteFactResult{}, fmt.Errorf("WriteFactIfUnchanged: expectBlob is required")
	}
	commitHash, blobHash, err := fi.writeFileExact(ctx, branch, strings.ToLower(path), content, message, operation, expectBlob)
	if err != nil {
		return WriteFactResult{}, err
	}
	return WriteFactResult{CommitHash: commitHash, BlobHash: blobHash}, nil
}

// WriteRootFile writes a root-level, non-fact file (README.md, and any future
// sibling) preserving the case of path. Root-level only: a nested path would
// otherwise be a way around fact-path normalization.
func (fi *factIndex) WriteRootFile(ctx context.Context, branch, path, content, message, operation string) (WriteFactResult, error) {
	if strings.Contains(path, "/") {
		return WriteFactResult{}, fmt.Errorf("store: WriteRootFile: %q is not root-level", path)
	}
	commitHash, blobHash, err := fi.writeFileExact(ctx, branch, path, content, message, operation, "")
	if err != nil {
		return WriteFactResult{}, err
	}
	return WriteFactResult{CommitHash: commitHash, BlobHash: blobHash}, nil
}

// DeleteFact deletes a fact. Index sync happens inside deleteFile via
// rh.notifyCommit.
func (fi *factIndex) DeleteFact(ctx context.Context, branch, path, message string) (string, error) {
	commitHash, err := fi.deleteFile(ctx, branch, path, message, "retract")
	if err != nil {
		return "", fmt.Errorf("DeleteFact git: %w", err)
	}
	return commitHash, nil
}

// BatchWriteFacts writes and deletes multiple facts in a single commit. Index
// sync happens inside batchWrite via rh.notifyCommit.
func (fi *factIndex) BatchWriteFacts(ctx context.Context, branch string, files map[string]string, deletes []string, message, operation string) (commitHash string, blobHashes map[string]string, err error) {
	commitHash, blobHashes, err = fi.batchWrite(ctx, branch, files, deletes, nil, message, operation)
	return
}

// BatchWriteFactsMustExist is BatchWriteFacts with a precondition: every path
// in mustExist must be present at the branch tip, checked inside the branch
// write lock. Otherwise nothing is written and the error is a
// *RetractMissingError naming the absent paths. The F04 move passes its
// retracted paths here.
func (fi *factIndex) BatchWriteFactsMustExist(ctx context.Context, branch string, files map[string]string, deletes, mustExist []string, message, operation string) (commitHash string, blobHashes map[string]string, err error) {
	return fi.batchWrite(ctx, branch, files, deletes, mustExist, message, operation)
}

// tag creates a lightweight tag ref at the tip of branch.
func (fi *factIndex) tag(ctx context.Context, branch, name string) error {
	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return fmt.Errorf("tag: ref: %w", err)
	}

	tagRefName := plumbing.NewTagReferenceName(name)
	return fi.rh.gits.SetReference(plumbing.NewHashReference(tagRefName, headHash))
}

// tagsContaining returns tag names whose target is reachable from hash.
func (fi *factIndex) tagsContaining(ctx context.Context, hash string) ([]string, error) {
	targetHash := plumbing.NewHash(hash)

	// Build set of all commits reachable from targetHash (one walk).
	reachable := make(map[plumbing.Hash]bool)
	logIter, err := fi.rh.repo.Log(&gogit.LogOptions{From: targetHash})
	if err != nil {
		return nil, fmt.Errorf("tagsContaining: log from target: %w", err)
	}
	_ = logIter.ForEach(func(c *object.Commit) error {
		reachable[c.Hash] = true
		return nil
	})
	logIter.Close()

	refIter, err := fi.rh.gits.IterReferences()
	if err != nil {
		return nil, fmt.Errorf("tagsContaining: iter refs: %w", err)
	}
	defer refIter.Close()

	var tags []string
	err = refIter.ForEach(func(ref *plumbing.Reference) error {
		if !strings.HasPrefix(ref.Name().String(), "refs/tags/") {
			return nil
		}
		if reachable[ref.Hash()] {
			tagName := strings.TrimPrefix(ref.Name().String(), "refs/tags/")
			tags = append(tags, tagName)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("tagsContaining: %w", err)
	}

	sort.Strings(tags)
	return tags, nil
}
