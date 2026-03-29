package store

import (
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
func (idx *Index) EnsureBranch(name, gitRef string) (int64, error) {
	if id, ok := idx.branches.get(name); ok {
		return id, nil
	}

	_, err := idx.db.Exec(
		`INSERT OR IGNORE INTO branches(name, git_ref) VALUES (?, ?)`,
		name, gitRef,
	)
	if err != nil {
		return 0, fmt.Errorf("ensure branch: %w", err)
	}

	var id int64
	err = idx.db.QueryRow(`SELECT id FROM branches WHERE name = ?`, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure branch lookup: %w", err)
	}

	idx.branches.set(name, id)
	return id, nil
}

// BranchID returns the ID for a branch name, using the cache when possible.
// Returns an error if the branch does not exist.
func (idx *Index) BranchID(name string) (int64, error) {
	if id, ok := idx.branches.get(name); ok {
		return id, nil
	}

	var id int64
	err := idx.db.QueryRow(`SELECT id FROM branches WHERE name = ?`, name).Scan(&id)
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
func (idx *Index) ListBranches() ([]Branch, error) {
	rows, err := idx.db.Query(`SELECT id, name, git_ref FROM branches ORDER BY name`)
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
