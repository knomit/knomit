package store

import (
	"database/sql"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-billy/v5/memfs"
)

// TestDB returns the underlying *sql.DB for test setup and assertions.
// Only available in test builds.
func (idx *Index) TestDB() *sql.DB { return idx.db }

// TestDB returns the underlying *sql.DB for test setup and assertions.
// Only available in test builds.
func (s *Service) TestDB() *sql.DB { return s.db }

// TestOpenRepo opens the go-git repo on the Service using the shared storer.
// Only available in test builds. Call this after an external git.InitWithStorer
// populates the storer, so that Service.DeleteFile and similar methods work.
func (s *Service) TestOpenRepo() error {
	repo, err := gogit.Open(s.gits, memfs.New())
	if err != nil {
		return err
	}
	s.repo = repo
	return nil
}
