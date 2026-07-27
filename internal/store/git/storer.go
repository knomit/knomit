package git

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage"

	storemigrate "knomit/internal/store/migrate"
)

var _ storer.EncodedObjectStorer = (*Storer)(nil)
var _ storage.Storer = (*Storer)(nil)

// Storer implements go-git's storage.Storer over a shared SQLite *sql.DB.
// The caller (store.Service) owns the database lifecycle unless the Storer
// was created by NewMemoryStorer, in which case Close() must be called.
type Storer struct {
	db        *sql.DB
	ownsDB    bool
	commitLog atomic.Bool
	modules   map[string]*Storer
}

// execer is the common interface between *sql.DB and *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// NewStorer wraps an existing *sql.DB. Schema must already be applied.
func NewStorer(db *sql.DB) *Storer {
	return &Storer{db: db}
}

// NewMemoryStorer opens an in-memory SQLite database, applies the core schema
// migrations (standard tables only, no vec0 virtual table), and returns a
// Storer backed by it. The caller must call s.Close() when done.
func NewMemoryStorer() (*Storer, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("storegit.NewMemoryStorer: open: %w", err)
	}
	if err := storemigrate.Core(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("storegit.NewMemoryStorer: %w", err)
	}
	return &Storer{db: db, ownsDB: true}, nil
}

// Close releases the database connection if this Storer opened it
// (i.e. was created by NewMemoryStorer). It is a no-op otherwise.
func (s *Storer) Close() error {
	if s.ownsDB {
		return s.db.Close()
	}
	return nil
}

// DB returns the underlying SQL DB. Used by integrity tests and the verify tool
// for read-only schema introspection.
func (s *Storer) DB() *sql.DB { return s.db }

// conn returns the raw *sql.DB as an execer. Used by go-git interface methods
// which don't accept context. For context-aware callers, use connCtx instead.
func (s *Storer) conn() execer {
	return s.db
}

// connCtx returns the tx from context if present, otherwise the raw db.
func (s *Storer) connCtx(ctx context.Context) CtxExecer {
	return Conn(ctx, s.db)
}

// --- EncodedObjectStorer ---

// NewEncodedObject returns a new in-memory object ready to be written.
func (s *Storer) NewEncodedObject() plumbing.EncodedObject {
	return &plumbing.MemoryObject{}
}

// SetEncodedObject persists obj to the objects table and returns its hash.
// Delta object types (REFDeltaObject, OFSDeltaObject) are not supported.
func (s *Storer) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	t := obj.Type()
	if t == plumbing.REFDeltaObject || t == plumbing.OFSDeltaObject {
		return plumbing.ZeroHash, fmt.Errorf("storegit: delta objects not supported")
	}
	r, err := obj.Reader()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("storegit: SetEncodedObject reader: %w", err)
	}
	defer r.Close()

	data, err := io.ReadAll(r)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("storegit: SetEncodedObject read: %w", err)
	}

	hash := obj.Hash()
	_, err = s.conn().Exec(
		`INSERT OR IGNORE INTO objects (hash, type, size, data) VALUES (?, ?, ?, ?)`,
		hash.String(), int(obj.Type()), obj.Size(), data,
	)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("storegit: SetEncodedObject insert: %w", err)
	}
	return hash, nil
}

// EncodedObject retrieves a single object by type and hash.
func (s *Storer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	var row *sql.Row
	if t == plumbing.AnyObject {
		row = s.conn().QueryRow(`SELECT type, size, data FROM objects WHERE hash=? LIMIT 1`, h.String())
	} else {
		row = s.conn().QueryRow(`SELECT type, size, data FROM objects WHERE hash=? AND type=?`, h.String(), int(t))
	}
	var typ int
	var size int64
	var data []byte
	if err := row.Scan(&typ, &size, &data); err == sql.ErrNoRows {
		return nil, plumbing.ErrObjectNotFound
	} else if err != nil {
		return nil, err
	}
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.ObjectType(typ))
	obj.SetSize(size)
	obj.Write(data)
	return obj, nil
}

// IterEncodedObjects returns an iterator over all objects of type t.
func (s *Storer) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	var rows *sql.Rows
	var err error
	if t == plumbing.AnyObject {
		rows, err = s.conn().Query(`SELECT hash, type, size, data FROM objects`)
	} else {
		rows, err = s.conn().Query(`SELECT hash, type, size, data FROM objects WHERE type=?`, int(t))
	}
	if err != nil {
		return nil, err
	}
	return &objectIter{rows: rows}, nil
}

// HasEncodedObject returns nil if the object exists, plumbing.ErrObjectNotFound otherwise.
func (s *Storer) HasEncodedObject(h plumbing.Hash) error {
	var count int
	err := s.conn().QueryRow(
		`SELECT COUNT(*) FROM objects WHERE hash = ?`, h.String(),
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("storegit: HasEncodedObject: %w", err)
	}
	if count == 0 {
		return plumbing.ErrObjectNotFound
	}
	return nil
}

// EncodedObjectSize returns the stored size of the object with the given hash.
func (s *Storer) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	var size int64
	err := s.conn().QueryRow(
		`SELECT size FROM objects WHERE hash = ? LIMIT 1`, h.String(),
	).Scan(&size)
	if err == sql.ErrNoRows {
		return 0, plumbing.ErrObjectNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("storegit: EncodedObjectSize: %w", err)
	}
	return size, nil
}

// AddAlternate is not supported by this backend.
func (s *Storer) AddAlternate(remote string) error {
	return fmt.Errorf("storegit: alternates not supported")
}

// DeleteObjectForTest removes an object from the SQLite-backed object store.
// Test-only escape hatch for integrity-check tests.
func (s *Storer) DeleteObjectForTest(hash plumbing.Hash) error {
	_, err := s.db.Exec(`DELETE FROM objects WHERE hash = ?`, hash.String())
	return err
}

// --- objectIter ---

type objectIter struct {
	rows *sql.Rows
}

func (it *objectIter) Next() (plumbing.EncodedObject, error) {
	if !it.rows.Next() {
		return nil, io.EOF
	}
	var hashStr string
	var typ int
	var size int64
	var data []byte
	if err := it.rows.Scan(&hashStr, &typ, &size, &data); err != nil {
		it.rows.Close()
		return nil, err
	}
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.ObjectType(typ))
	obj.SetSize(size)
	obj.Write(data)
	return obj, nil
}

func (it *objectIter) ForEach(fn func(plumbing.EncodedObject) error) error {
	defer it.rows.Close()
	for {
		obj, err := it.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(obj); err != nil {
			if err == storer.ErrStop {
				return nil
			}
			return err
		}
	}
}

func (it *objectIter) Close() {
	it.rows.Close()
}
