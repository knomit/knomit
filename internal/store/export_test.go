package store

import "database/sql"

// TestDB returns the underlying *sql.DB for test setup and assertions.
// Only available in test builds.
func (idx *Index) TestDB() *sql.DB { return idx.db }

// TestDB returns the underlying *sql.DB for test setup and assertions.
// Only available in test builds.
func (s *Service) TestDB() *sql.DB { return s.db }
