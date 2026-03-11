package gitstorer

import (
	"database/sql"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	_ "github.com/mattn/go-sqlite3"
)

const schema = `
CREATE TABLE IF NOT EXISTS objects (
    hash TEXT NOT NULL,
    type INTEGER NOT NULL,
    size INTEGER NOT NULL,
    data BLOB NOT NULL,
    PRIMARY KEY (hash, type)
);
CREATE TABLE IF NOT EXISTS refs (
    name        TEXT PRIMARY KEY,
    target      TEXT NOT NULL,
    is_symbolic INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS kv (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);
`

// Storer implements go-git's storage.Storer over SQLite.
type Storer struct {
	db *sql.DB
}

// New opens (or creates) a SQLite database at path and initialises the schema.
// Use ":memory:" for an in-memory database (useful in tests).
func New(path string) (*Storer, error) {
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("gitstorer: open db: %w", err)
	}
	s := &Storer{db: db}
	if err := s.createSchema(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Storer) Close() error {
	return s.db.Close()
}

func (s *Storer) createSchema() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("gitstorer: create schema: %w", err)
	}
	return nil
}

func (s *Storer) migrate() error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO kv (key, value) VALUES ('schema_version', '1')`)
	if err != nil {
		return fmt.Errorf("gitstorer: migrate: %w", err)
	}
	return nil
}

// --- EncodedObjectStorer ---

// NewEncodedObject returns a new in-memory object ready to be written.
func (s *Storer) NewEncodedObject() plumbing.EncodedObject {
	return &plumbing.MemoryObject{}
}

// SetEncodedObject persists obj to the objects table and returns its hash.
func (s *Storer) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	r, err := obj.Reader()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitstorer: SetEncodedObject reader: %w", err)
	}
	defer r.Close()

	data, err := io.ReadAll(r)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitstorer: SetEncodedObject read: %w", err)
	}

	hash := obj.Hash()
	_, err = s.db.Exec(
		`INSERT OR IGNORE INTO objects (hash, type, size, data) VALUES (?, ?, ?, ?)`,
		hash.String(),
		int(obj.Type()),
		obj.Size(),
		data,
	)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitstorer: SetEncodedObject insert: %w", err)
	}
	return hash, nil
}

// EncodedObject retrieves a single object by type and hash.
func (s *Storer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	var objType int
	var size int64
	var data []byte

	err := s.db.QueryRow(
		`SELECT type, size, data FROM objects WHERE hash = ? AND type = ?`,
		h.String(), int(t),
	).Scan(&objType, &size, &data)
	if err == sql.ErrNoRows {
		return nil, plumbing.ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("gitstorer: EncodedObject query: %w", err)
	}

	mo := &plumbing.MemoryObject{}
	mo.SetType(plumbing.ObjectType(objType))
	mo.SetSize(size)
	if _, err := mo.Write(data); err != nil {
		return nil, fmt.Errorf("gitstorer: EncodedObject write mem: %w", err)
	}
	return mo, nil
}

// IterEncodedObjects returns an iterator over all objects of type t.
func (s *Storer) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	rows, err := s.db.Query(
		`SELECT type, size, data FROM objects WHERE type = ?`,
		int(t),
	)
	if err != nil {
		return nil, fmt.Errorf("gitstorer: IterEncodedObjects query: %w", err)
	}
	return &objectIter{rows: rows}, nil
}

// HasEncodedObject returns nil if the object exists, plumbing.ErrObjectNotFound otherwise.
func (s *Storer) HasEncodedObject(h plumbing.Hash) error {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM objects WHERE hash = ?`, h.String(),
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("gitstorer: HasEncodedObject: %w", err)
	}
	if count == 0 {
		return plumbing.ErrObjectNotFound
	}
	return nil
}

// EncodedObjectSize returns the stored size of the object with the given hash.
func (s *Storer) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	var size int64
	err := s.db.QueryRow(
		`SELECT size FROM objects WHERE hash = ? LIMIT 1`, h.String(),
	).Scan(&size)
	if err == sql.ErrNoRows {
		return 0, plumbing.ErrObjectNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("gitstorer: EncodedObjectSize: %w", err)
	}
	return size, nil
}

// --- Stubs for full storer interface ---

// PackfileWriter is not supported by this backend.
func (s *Storer) PackfileWriter() (io.WriteCloser, error) {
	return nil, fmt.Errorf("packfile not supported")
}

// DeltaObject delegates to EncodedObject.
func (s *Storer) DeltaObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	return s.EncodedObject(t, h)
}

// --- objectIter ---

type objectIter struct {
	rows *sql.Rows
}

func (it *objectIter) Next() (plumbing.EncodedObject, error) {
	if !it.rows.Next() {
		if err := it.rows.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}

	var objType int
	var size int64
	var data []byte
	if err := it.rows.Scan(&objType, &size, &data); err != nil {
		return nil, fmt.Errorf("gitstorer: objectIter scan: %w", err)
	}

	mo := &plumbing.MemoryObject{}
	mo.SetType(plumbing.ObjectType(objType))
	mo.SetSize(size)
	if _, err := mo.Write(data); err != nil {
		return nil, fmt.Errorf("gitstorer: objectIter write mem: %w", err)
	}
	return mo, nil
}

func (it *objectIter) ForEach(fn func(plumbing.EncodedObject) error) error {
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
