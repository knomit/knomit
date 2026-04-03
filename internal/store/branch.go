package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	storegit "knomit/internal/store/git"
)

// Branch holds metadata about an indexed branch.
type Branch struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	GitRef string `json:"git_ref"`
}

// branchCache is a simple thread-safe name→ID cache.
type branchCache struct {
	mu     sync.RWMutex
	byName map[string]int64
}

func newBranchCache() *branchCache {
	return &branchCache{byName: make(map[string]int64)}
}

func (c *branchCache) get(name string) (int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.byName[name]
	return id, ok
}

func (c *branchCache) set(name string, id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byName[name] = id
}

func (c *branchCache) remove(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byName, name)
}

// repoHandler owns the database handle, branch cache, and branch operations.
type repoHandler struct {
	db       *sql.DB
	cache    *branchCache
	onDrop   func(context.Context) error
	gits     *storegit.Storer
	repo     *gogit.Repository // nil until OpenRepo/InitRepo/Clone called
	configMu sync.Mutex        // guards ConfigureRemote / remote wiring
}

// Compile-time assertions.
var _ BranchIndex = (*repoHandler)(nil)
var _ gitReader    = (*repoHandler)(nil)

func newRepoHandler(db *sql.DB, gits *storegit.Storer) *repoHandler {
	return &repoHandler{db: db, cache: newBranchCache(), gits: gits}
}

// EnsureBranch creates the branch if it doesn't exist, returns its ID.
func (rh *repoHandler) EnsureBranch(ctx context.Context, name, gitRef string) (int64, error) {
	if id, ok := rh.cache.get(name); ok {
		return id, nil
	}

	_, err := conn(ctx, rh.db).ExecContext(ctx,
		`INSERT OR IGNORE INTO branches(name, git_ref) VALUES (?, ?)`,
		name, gitRef,
	)
	if err != nil {
		return 0, fmt.Errorf("ensure branch: %w", err)
	}

	var id int64
	err = conn(ctx, rh.db).QueryRowContext(ctx, `SELECT id FROM branches WHERE name = ?`, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure branch lookup: %w", err)
	}

	rh.cache.set(name, id)
	return id, nil
}

// branchID returns the ID for a branch name, using the cache when possible.
// Returns an error if the branch does not exist.
func (rh *repoHandler) branchID(ctx context.Context, name string) (int64, error) {
	if id, ok := rh.cache.get(name); ok {
		return id, nil
	}

	var id int64
	err := conn(ctx, rh.db).QueryRowContext(ctx, `SELECT id FROM branches WHERE name = ?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("branch %q not found", name)
	}
	if err != nil {
		return 0, fmt.Errorf("branch lookup: %w", err)
	}

	rh.cache.set(name, id)
	return id, nil
}

// MergeBranch copies all branch_facts entries from src to dst.
// Conflicting paths (same path on both branches) are overwritten with src's version.
func (rh *repoHandler) MergeBranch(ctx context.Context, src, dst string) error {
	srcID, err := rh.branchID(ctx, src)
	if err != nil {
		return fmt.Errorf("merge: src %w", err)
	}
	dstID, err := rh.EnsureBranch(ctx, dst, "refs/heads/"+dst)
	if err != nil {
		return fmt.Errorf("merge: dst %w", err)
	}

	_, err = conn(ctx, rh.db).ExecContext(ctx,
		`INSERT OR REPLACE INTO branch_facts(branch_id, path, fact_id, commit_hash)
		 SELECT ?, path, fact_id, commit_hash
		 FROM branch_facts WHERE branch_id = ?`,
		dstID, srcID,
	)
	if err != nil {
		return fmt.Errorf("merge branch_facts: %w", err)
	}
	return nil
}

// DropBranch removes a branch and all its branch_facts entries, then runs GC.
func (rh *repoHandler) DropBranch(ctx context.Context, name string) error {
	id, err := rh.branchID(ctx, name)
	if err != nil {
		return fmt.Errorf("drop branch: %w", err)
	}

	if _, err := conn(ctx, rh.db).ExecContext(ctx, `DELETE FROM branch_facts WHERE branch_id = ?`, id); err != nil {
		return fmt.Errorf("drop branch_facts: %w", err)
	}
	if _, err := conn(ctx, rh.db).ExecContext(ctx, `DELETE FROM branches WHERE id = ?`, id); err != nil {
		return fmt.Errorf("drop branch row: %w", err)
	}

	rh.cache.remove(name)

	if rh.onDrop != nil {
		return rh.onDrop(ctx)
	}
	return nil
}

// ListBranches returns all registered branches.
func (rh *repoHandler) ListBranches(ctx context.Context) ([]Branch, error) {
	rows, err := conn(ctx, rh.db).QueryContext(ctx, `SELECT id, name, git_ref FROM branches ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	defer rows.Close()

	var branches []Branch
	for rows.Next() {
		var b Branch
		if err := rows.Scan(&b.ID, &b.Name, &b.GitRef); err != nil {
			return nil, fmt.Errorf("list branches scan: %w", err)
		}
		branches = append(branches, b)
	}
	return branches, rows.Err()
}

// ── git read methods ──────────────────────────────────────────────────────────

// resolveRef returns the commit hash at the tip of branch.
func (rh *repoHandler) resolveRef(ctx context.Context, branch string) (plumbing.Hash, error) {
	ref, err := rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolveRef %q: %w", branch, err)
	}
	return ref.Hash(), nil
}

// readFileAtCommitHash reads the content of path from a specific commit.
// If the exact path is not found, it falls back to a case-insensitive tree
// walk so that normalised (lowercase) index paths resolve correctly against
// pre-normalisation commits that stored paths with mixed case.
func (rh *repoHandler) readFileAtCommitHash(ctx context.Context, path, commitHash string) (string, error) {
	hash := plumbing.NewHash(commitHash)
	commit, err := rh.repo.CommitObject(hash)
	if err != nil {
		return "", fmt.Errorf("readFileAtCommitHash: commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("readFileAtCommitHash: tree: %w", err)
	}
	if f, err := tree.File(path); err == nil {
		return f.Contents()
	}
	// Exact lookup failed — try case-insensitive walk.
	content, err := treeFileInsensitive(rh.repo, tree, path)
	if err != nil {
		return "", fmt.Errorf("readFileAtCommitHash: file %q not found (case-insensitive): %w", path, err)
	}
	return content, nil
}

// HeadCommit returns the hash of the tip commit of branch as a hex string.
func (rh *repoHandler) HeadCommit(ctx context.Context, branch string) (string, error) {
	hash, err := rh.resolveRef(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("HeadCommit: %w", err)
	}
	return hash.String(), nil
}

// readFileWithHash returns both the file content and the blob hash for the given path.
func (rh *repoHandler) readFileWithHash(ctx context.Context, branch, path string) (string, string, error) {
	headHash, err := rh.resolveRef(ctx, branch)
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: ref: %w", err)
	}
	commit, err := rh.repo.CommitObject(headHash)
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: tree: %w", err)
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: entry %s: %w", path, err)
	}
	blob, err := rh.repo.BlobObject(entry.Hash)
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: blob: %w", err)
	}
	r, err := blob.Reader()
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: reader: %w", err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return "", "", fmt.Errorf("readFileWithHash: read: %w", err)
	}
	return string(b), entry.Hash.String(), nil
}

// readFile reads the content of path from the tip of branch.
func (rh *repoHandler) readFile(ctx context.Context, branch, path string) (string, error) {
	content, _, err := rh.readFileWithHash(ctx, branch, path)
	return content, err
}

// readFileAtCommit reads the content of path at the given commit.
// branch is accepted for interface consistency; the commit hash uniquely
// identifies the version without branch resolution.
func (rh *repoHandler) readFileAtCommit(ctx context.Context, branch, path, commitHash string) (string, error) {
	return rh.readFileAtCommitHash(ctx, path, commitHash)
}

// pathHashSorter sorts two parallel slices (paths and hashes) together by path.
type pathHashSorter struct{ paths, hashes []string }

func (ps pathHashSorter) Len() int           { return len(ps.paths) }
func (ps pathHashSorter) Less(i, j int) bool { return ps.paths[i] < ps.paths[j] }
func (ps pathHashSorter) Swap(i, j int) {
	ps.paths[i], ps.paths[j] = ps.paths[j], ps.paths[i]
	ps.hashes[i], ps.hashes[j] = ps.hashes[j], ps.hashes[i]
}

// ListAllWithHash returns all .md files at the tip of branch with their blob hashes.
// Single tree walk — no per-file I/O.
func (rh *repoHandler) ListAllWithHash(ctx context.Context, branch string) ([]string, []string, error) {
	headHash, err := rh.resolveRef(ctx, branch)
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: ref: %w", err)
	}

	commit, err := rh.repo.CommitObject(headHash)
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: commit: %w", err)
	}

	fileIter, err := commit.Files()
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: files: %w", err)
	}
	defer fileIter.Close()

	var paths, blobHashes []string
	err = fileIter.ForEach(func(f *object.File) error {
		if strings.HasSuffix(f.Name, ".md") {
			paths = append(paths, f.Name)
			blobHashes = append(blobHashes, f.Hash.String())
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ListAllWithHash: iterate: %w", err)
	}

	sort.Sort(pathHashSorter{paths, blobHashes})
	return paths, blobHashes, nil
}

// ListAll returns paths of all .md files at the tip of branch.
func (rh *repoHandler) ListAll(ctx context.Context, branch string) ([]string, error) {
	paths, _, err := rh.ListAllWithHash(ctx, branch)
	return paths, err
}

// LastCommitForPath returns the hash of the most recent non-merge commit
// that touched path. Merges are skipped because they duplicate authoring
// commits from the merged branch.
func (rh *repoHandler) LastCommitForPath(ctx context.Context, branch, path string) (string, error) {
	headHash, err := rh.resolveRef(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("LastCommitForPath: ref: %w", err)
	}

	logIter, err := rh.repo.Log(&gogit.LogOptions{
		From:     headHash,
		FileName: &path,
		Order:    gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return "", fmt.Errorf("LastCommitForPath: log: %w", err)
	}
	defer logIter.Close()

	for {
		c, err := logIter.Next()
		if err != nil {
			return "", fmt.Errorf("LastCommitForPath: %q: no commit found", path)
		}
		// Skip merge commits (more than one parent).
		if c.NumParents() <= 1 {
			return c.Hash.String(), nil
		}
	}
}

// DiffFiles returns paths added/modified/deleted between fromCommit and the tip of branch.
// Only .md files are returned. If fromCommit is empty, diffs from empty tree.
func (rh *repoHandler) DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error) {
	headHash, err := rh.resolveRef(ctx, branch)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: ref: %w", err)
	}

	toCommit, err := rh.repo.CommitObject(headHash)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: to commit: %w", err)
	}
	toTree, err := toCommit.Tree()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: to tree: %w", err)
	}

	var fromTree *object.Tree
	if fromCommit != "" {
		fromHash := plumbing.NewHash(fromCommit)
		fc, err := rh.repo.CommitObject(fromHash)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("DiffFiles: from commit: %w", err)
		}
		fromTree, err = fc.Tree()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("DiffFiles: from tree: %w", err)
		}
	}

	changes, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: diff tree: %w", err)
	}

	for _, ch := range changes {
		from := ch.From.Name
		to := ch.To.Name

		switch {
		case from == "" && to != "":
			if strings.HasSuffix(to, ".md") {
				added = append(added, to)
			}
		case from != "" && to == "":
			if strings.HasSuffix(from, ".md") {
				deleted = append(deleted, from)
			}
		default:
			if strings.HasSuffix(to, ".md") {
				modified = append(modified, to)
			}
		}
	}

	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(deleted)
	return added, modified, deleted, nil
}
