package gitstorer

import (
	"bytes"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage"
)

var _ config.ConfigStorer = (*Storer)(nil)
var _ storer.IndexStorer = (*Storer)(nil)
var _ storer.ShallowStorer = (*Storer)(nil)
var _ storage.ModuleStorer = (*Storer)(nil)

// kvGet retrieves a value from the kv table by key.
// Returns (nil, nil) when the key is not found.
func (s *Storer) kvGet(key string) ([]byte, error) {
	var val []byte
	err := s.db.QueryRow(`SELECT value FROM kv WHERE key=?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return val, err
}

// kvSet upserts a value into the kv table.
func (s *Storer) kvSet(key string, val []byte) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, key, val,
	)
	return err
}

// --- ConfigStorer ---

// Config returns the repository configuration.
// If no config has been stored, an empty Config is returned.
func (s *Storer) Config() (*config.Config, error) {
	data, err := s.kvGet("config")
	if err != nil {
		return nil, err
	}
	cfg := config.NewConfig()
	if data == nil {
		return cfg, nil
	}
	if err := cfg.Unmarshal(data); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SetConfig persists the repository configuration.
// Returns an error if the configuration is invalid.
func (s *Storer) SetConfig(cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := cfg.Marshal()
	if err != nil {
		return err
	}
	return s.kvSet("config", data)
}

// --- IndexStorer ---

// Index returns the repository index.
// If no index has been stored, an empty Index with Version 2 is returned.
// ModTime is restored from the "index:modtime" KV entry if present.
func (s *Storer) Index() (*index.Index, error) {
	data, err := s.kvGet("index")
	if err != nil {
		return nil, err
	}
	idx := &index.Index{Version: 2}
	if data == nil {
		return idx, nil
	}
	dec := index.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(idx); err != nil {
		return nil, err
	}
	// Restore ModTime stored at SetIndex time.
	if tsData, err := s.kvGet("index:modtime"); err == nil && tsData != nil {
		if ns, err := strconv.ParseInt(strings.TrimSpace(string(tsData)), 10, 64); err == nil {
			idx.ModTime = time.Unix(0, ns)
		}
	}
	return idx, nil
}

// SetIndex persists the repository index.
// ModTime is set to now (mirroring memory storage behaviour) and stored
// separately so it survives the encode/decode round-trip.
func (s *Storer) SetIndex(idx *index.Index) error {
	idx.ModTime = time.Now()
	if err := s.kvSet("index:modtime", []byte(strconv.FormatInt(idx.ModTime.UnixNano(), 10))); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := index.NewEncoder(&buf)
	if err := enc.Encode(idx); err != nil {
		return err
	}
	return s.kvSet("index", buf.Bytes())
}

// --- ShallowStorer ---

// Shallow returns the list of shallow commit hashes.
// If none have been stored, a nil slice is returned.
func (s *Storer) Shallow() ([]plumbing.Hash, error) {
	data, err := s.kvGet("shallow")
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	hashes := make([]plumbing.Hash, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		hashes = append(hashes, plumbing.NewHash(line))
	}
	return hashes, nil
}

// SetShallow persists the list of shallow commit hashes.
func (s *Storer) SetShallow(commits []plumbing.Hash) error {
	if len(commits) == 0 {
		return s.kvSet("shallow", []byte{})
	}
	var sb strings.Builder
	for i, h := range commits {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(h.String())
	}
	return s.kvSet("shallow", []byte(sb.String()))
}

// --- ModuleStorer ---

// Module returns a Storer for the named submodule, backed by an in-memory
// SQLite database. The submodule name is recorded in the kv table.
func (s *Storer) Module(name string) (storage.Storer, error) {
	key := "module:" + name
	if err := s.kvSet(key, []byte("1")); err != nil {
		return nil, err
	}
	return New(":memory:")
}
