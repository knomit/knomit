package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
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
	db     *sql.DB
	cache  *branchCache
	onDrop func(context.Context) error
}

// Compile-time assertion.
var _ BranchIndex = (*repoHandler)(nil)

func newRepoHandler(db *sql.DB) *repoHandler {
	return &repoHandler{db: db, cache: newBranchCache()}
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

// ListBranches returns all registered branches.
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
