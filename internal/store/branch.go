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

// EnsureBranch creates the branch if it doesn't exist, returns its ID.
func (idx *Index) EnsureBranch(ctx context.Context, name, gitRef string) (int64, error) {
	if id, ok := idx.branches.get(name); ok {
		return id, nil
	}

	_, err := conn(ctx, idx.db).ExecContext(ctx,
		`INSERT OR IGNORE INTO branches(name, git_ref) VALUES (?, ?)`,
		name, gitRef,
	)
	if err != nil {
		return 0, fmt.Errorf("ensure branch: %w", err)
	}

	var id int64
	err = conn(ctx, idx.db).QueryRowContext(ctx, `SELECT id FROM branches WHERE name = ?`, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure branch lookup: %w", err)
	}

	idx.branches.set(name, id)
	return id, nil
}

// branchID returns the ID for a branch name, using the cache when possible.
// Returns an error if the branch does not exist.
func (idx *Index) branchID(ctx context.Context, name string) (int64, error) {
	if id, ok := idx.branches.get(name); ok {
		return id, nil
	}

	var id int64
	err := conn(ctx, idx.db).QueryRowContext(ctx, `SELECT id FROM branches WHERE name = ?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("branch %q not found", name)
	}
	if err != nil {
		return 0, fmt.Errorf("branch lookup: %w", err)
	}

	idx.branches.set(name, id)
	return id, nil
}

// ListBranches returns all registered branches.
// MergeBranch copies all branch_facts entries from src to dst.
// Conflicting paths (same path on both branches) are overwritten with src's version.
func (idx *Index) MergeBranch(ctx context.Context, src, dst string) error {
	srcID, err := idx.branchID(ctx, src)
	if err != nil {
		return fmt.Errorf("merge: src %w", err)
	}
	dstID, err := idx.EnsureBranch(ctx, dst, "refs/heads/"+dst)
	if err != nil {
		return fmt.Errorf("merge: dst %w", err)
	}

	_, err = conn(ctx, idx.db).ExecContext(ctx,
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
func (idx *Index) DropBranch(ctx context.Context, name string) error {
	id, err := idx.branchID(ctx, name)
	if err != nil {
		return fmt.Errorf("drop branch: %w", err)
	}

	if _, err := conn(ctx, idx.db).ExecContext(ctx, `DELETE FROM branch_facts WHERE branch_id = ?`, id); err != nil {
		return fmt.Errorf("drop branch_facts: %w", err)
	}
	if _, err := conn(ctx, idx.db).ExecContext(ctx, `DELETE FROM branches WHERE id = ?`, id); err != nil {
		return fmt.Errorf("drop branch row: %w", err)
	}

	idx.branches.remove(name)

	return idx.GC(ctx)
}

// ListBranches returns all registered branches.
func (idx *Index) ListBranches(ctx context.Context) ([]Branch, error) {
	rows, err := conn(ctx, idx.db).QueryContext(ctx, `SELECT id, name, git_ref FROM branches ORDER BY name`)
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
