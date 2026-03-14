package git_test

import (
	"database/sql"
	"testing"

	gogitconfig "github.com/go-git/go-git/v5/config"
	storagetestutils "github.com/go-git/go-git/v5/storage/test"
	_ "github.com/mattn/go-sqlite3"
	"gopkg.in/check.v1"

	storegit "knomit/internal/store/git"
)

// storerSuite wraps BaseStorageSuite so it can be registered with check.v1.
type storerSuite struct {
	storagetestutils.BaseStorageSuite
}

var _ = check.Suite(&storerSuite{})

func (s *storerSuite) SetUpTest(c *check.C) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		c.Fatal(err)
	}
	schema := `
CREATE TABLE IF NOT EXISTS objects (hash TEXT NOT NULL, type INTEGER NOT NULL, size INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY (hash, type));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target TEXT NOT NULL, is_symbolic INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value BLOB NOT NULL);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		c.Fatal(err)
	}
	st := storegit.NewStorer(db)
	s.BaseStorageSuite = storagetestutils.NewBaseStorageSuite(st)
}

// TestSetConfigAndConfig overrides the base suite's version because the base
// suite uses DeepEquals on RemoteConfig, which fails due to an unexported
// internal *format.Subsection pointer that differs after marshal/unmarshal.
// We verify the observable fields instead.
func (s *storerSuite) TestSetConfigAndConfig(c *check.C) {
	expected := gogitconfig.NewConfig()
	expected.Core.IsBare = true
	expected.Remotes["foo"] = &gogitconfig.RemoteConfig{
		Name: "foo",
		URLs: []string{"http://foo/bar.git"},
	}

	err := s.Storer.SetConfig(expected)
	c.Assert(err, check.IsNil)

	cfg, err := s.Storer.Config()
	c.Assert(err, check.IsNil)

	c.Assert(cfg.Core.IsBare, check.Equals, expected.Core.IsBare)
	c.Assert(cfg.Remotes["foo"].Name, check.Equals, expected.Remotes["foo"].Name)
	c.Assert(cfg.Remotes["foo"].URLs, check.DeepEquals, expected.Remotes["foo"].URLs)
}

// TestConformance runs all check.v1 tests via the standard testing.T bridge.
func TestConformance(t *testing.T) {
	check.TestingT(t)
}
