package gitstorer

import (
	"database/sql"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage"
)

var _ storer.ReferenceStorer = (*Storer)(nil)

// SetReference inserts or replaces a reference in the refs table.
func (s *Storer) SetReference(ref *plumbing.Reference) error {
	var target string
	var isSymbolic int
	if ref.Type() == plumbing.SymbolicReference {
		target = ref.Target().String()
		isSymbolic = 1
	} else {
		target = ref.Hash().String()
		isSymbolic = 0
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO refs (name, target, is_symbolic) VALUES (?, ?, ?)`,
		ref.Name().String(),
		target,
		isSymbolic,
	)
	return err
}

// CheckAndSetReference sets new if old matches the currently stored value.
// If old is non-nil and the current stored hash differs from old's hash,
// storage.ErrReferenceHasChanged is returned.
func (s *Storer) CheckAndSetReference(new, old *plumbing.Reference) error {
	if old != nil {
		cur, err := s.Reference(old.Name())
		if err != nil && err != plumbing.ErrReferenceNotFound {
			return err
		}
		if err == nil && cur.Hash() != old.Hash() {
			return storage.ErrReferenceHasChanged
		}
	}
	return s.SetReference(new)
}

// Reference retrieves a single reference by name.
func (s *Storer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	var target string
	var isSymbolic int
	err := s.db.QueryRow(
		`SELECT target, is_symbolic FROM refs WHERE name=?`, name.String(),
	).Scan(&target, &isSymbolic)
	if err == sql.ErrNoRows {
		return nil, plumbing.ErrReferenceNotFound
	}
	if err != nil {
		return nil, err
	}
	if isSymbolic == 1 {
		return plumbing.NewSymbolicReference(name, plumbing.ReferenceName(target)), nil
	}
	return plumbing.NewHashReference(name, plumbing.NewHash(target)), nil
}

// IterReferences returns an iterator over all stored references.
func (s *Storer) IterReferences() (storer.ReferenceIter, error) {
	rows, err := s.db.Query(`SELECT name, target, is_symbolic FROM refs`)
	if err != nil {
		return nil, err
	}
	return &refIter{rows: rows}, nil
}

// RemoveReference deletes the reference with the given name.
func (s *Storer) RemoveReference(name plumbing.ReferenceName) error {
	_, err := s.db.Exec(`DELETE FROM refs WHERE name=?`, name.String())
	return err
}

// CountLooseRefs returns the total number of stored references.
func (s *Storer) CountLooseRefs() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM refs`).Scan(&count)
	return count, err
}

// PackRefs is a no-op; all refs are already stored in SQLite.
func (s *Storer) PackRefs() error {
	return nil
}

// --- refIter ---

type refIter struct {
	rows *sql.Rows
}

func (it *refIter) Next() (*plumbing.Reference, error) {
	if !it.rows.Next() {
		return nil, io.EOF
	}
	var name, target string
	var isSymbolic int
	if err := it.rows.Scan(&name, &target, &isSymbolic); err != nil {
		it.rows.Close()
		return nil, err
	}
	refName := plumbing.ReferenceName(name)
	if isSymbolic == 1 {
		return plumbing.NewSymbolicReference(refName, plumbing.ReferenceName(target)), nil
	}
	return plumbing.NewHashReference(refName, plumbing.NewHash(target)), nil
}

func (it *refIter) ForEach(fn func(*plumbing.Reference) error) error {
	defer it.rows.Close()
	for {
		ref, err := it.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(ref); err != nil {
			if err == storer.ErrStop {
				return nil
			}
			return err
		}
	}
}

func (it *refIter) Close() {
	it.rows.Close()
}
