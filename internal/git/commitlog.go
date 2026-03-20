// commit_log population and append helpers.
// The commit_log table is a denormalized index of (commit_hash, path, committed_at, message)
// that enables O(1) activity aggregates and efficient path-history queries.
//
// Insertion order is oldest-first so that SQLite rowid directly reflects commit age:
// higher rowid = more recent commit. This lets ORDER BY rowid DESC serve as a
// stable tiebreaker when multiple commits share the same committed_at timestamp.
package git

import (
	"fmt"
	"io"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"
)

// parseOperation extracts the operation from an author email using the +tag subaddress convention.
// "agent+learn@agents.knomit.io" → "learn", "bob+learn@gmail.com" → "learn", "bob@gmail.com" → "".
func parseOperation(email string) string {
	plusIdx := strings.IndexByte(email, '+')
	if plusIdx < 0 {
		return ""
	}
	atIdx := strings.IndexByte(email, '@')
	if atIdx < 0 || atIdx < plusIdx {
		return ""
	}
	return email[plusIdx+1 : atIdx]
}

// firstLine returns the first line of s.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

type changedFile struct {
	path   string
	action string // "added", "modified", "deleted"
}

// changedFilesInCommit returns the .md files added/modified/deleted in c.
func changedFilesInCommit(c *object.Commit) ([]changedFile, error) {
	toTree, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("changedFilesInCommit: tree: %w", err)
	}
	var fromTree *object.Tree
	if c.NumParents() > 0 {
		parent, err := c.Parent(0)
		if err != nil {
			return nil, fmt.Errorf("changedFilesInCommit: parent: %w", err)
		}
		fromTree, err = parent.Tree()
		if err != nil {
			return nil, fmt.Errorf("changedFilesInCommit: parent tree: %w", err)
		}
	}
	changes, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return nil, fmt.Errorf("changedFilesInCommit: diff: %w", err)
	}
	var files []changedFile
	for _, ch := range changes {
		var path, action string
		switch {
		case ch.From.Name == "" && ch.To.Name != "":
			path, action = ch.To.Name, "added"
		case ch.From.Name != "" && ch.To.Name == "":
			path, action = ch.From.Name, "deleted"
		default:
			path, action = ch.To.Name, "modified"
		}
		if strings.HasSuffix(path, ".md") {
			files = append(files, changedFile{path: path, action: action})
		}
	}
	return files, nil
}

// populateCommitLog backfills commit_log from HEAD backwards.
// It stops at the first commit already present in the table so incremental
// calls (e.g. after sync) are cheap. Rows are inserted oldest-first to
// preserve rowid ordering (higher rowid = newer commit).
// Sets s.commitLog=true when the table is confirmed available.
func (s *Store) populateCommitLog() error {
	if s.db == nil {
		return nil
	}
	// Check table existence (absent in minimal test schemas).
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='commit_log'`).Scan(&n); err != nil || n == 0 {
		return nil
	}

	headRef, err := s.repo.Head()
	if err != nil {
		s.commitLog = true // table exists; empty repo is fine
		return nil
	}

	logIter, err := s.repo.Log(&gogit.LogOptions{
		From:  headRef.Hash(),
		Order: gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return fmt.Errorf("populateCommitLog: log: %w", err)
	}
	defer logIter.Close()

	type entry struct {
		hash        string
		ts          int64
		msg         string
		files       []changedFile
		authorEmail string
		operation   string
	}
	var toInsert []entry

	err = logIter.ForEach(func(c *object.Commit) error {
		// Stop at the first commit already indexed — everything older is already there.
		var cnt int
		if qerr := s.db.QueryRow(`SELECT COUNT(*) FROM commit_log WHERE commit_hash = ?`, c.Hash.String()).Scan(&cnt); qerr != nil {
			return qerr
		}
		if cnt > 0 {
			return io.EOF
		}
		files, err := changedFilesInCommit(c)
		if err != nil {
			return err
		}
		toInsert = append(toInsert, entry{
			hash:        c.Hash.String(),
			ts:          c.Committer.When.Unix(),
			msg:         firstLine(c.Message),
			files:       files,
			authorEmail: c.Author.Email,
			operation:   parseOperation(c.Author.Email),
		})
		return nil
	})
	if err != nil && err != io.EOF {
		return fmt.Errorf("populateCommitLog: walk: %w", err)
	}

	if len(toInsert) == 0 {
		s.commitLog = true
		return nil
	}

	// Reverse so oldest commit is inserted first → highest rowid = most recent.
	for i, j := 0, len(toInsert)-1; i < j; i, j = i+1, j-1 {
		toInsert[i], toInsert[j] = toInsert[j], toInsert[i]
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("populateCommitLog: begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO commit_log (commit_hash, path, committed_at, message, operation, author_email, action) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("populateCommitLog: prepare: %w", err)
	}
	defer stmt.Close()

	for _, e := range toInsert {
		for _, f := range e.files {
			if _, err := stmt.Exec(e.hash, f.path, e.ts, e.msg, e.operation, e.authorEmail, f.action); err != nil {
				return fmt.Errorf("populateCommitLog: insert: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("populateCommitLog: commit: %w", err)
	}

	log.Debug().Int("commits", len(toInsert)).Msg("commit_log: populated")
	s.commitLog = true
	return nil
}

// appendCommitLog inserts a single new commit into commit_log.
// New commits always get the highest rowid, preserving recency ordering.
// Errors are logged and swallowed — commit_log is an index, not source of truth.
func (s *Store) appendCommitLog(hash plumbing.Hash) {
	if !s.commitLog {
		return
	}
	c, err := s.repo.CommitObject(hash)
	if err != nil {
		log.Warn().Err(err).Str("hash", hash.String()[:8]).Msg("commit_log: get commit")
		return
	}
	files, err := changedFilesInCommit(c)
	if err != nil {
		log.Warn().Err(err).Str("hash", hash.String()[:8]).Msg("commit_log: changed files")
		return
	}
	authorEmail := c.Author.Email
	op := parseOperation(authorEmail)
	ts := c.Committer.When.Unix()
	msg := firstLine(c.Message)
	hashStr := hash.String()

	if len(files) <= 1 {
		// Single file — no need for transaction overhead.
		for _, f := range files {
			if _, err := s.db.Exec(
				`INSERT OR IGNORE INTO commit_log (commit_hash, path, committed_at, message, operation, author_email, action) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				hashStr, f.path, ts, msg, op, authorEmail, f.action,
			); err != nil {
				log.Warn().Err(err).Str("path", f.path).Msg("commit_log: insert")
			}
		}
		return
	}

	// Multi-file commit: batch in a single transaction with a prepared statement.
	tx, err := s.db.Begin()
	if err != nil {
		log.Warn().Err(err).Msg("commit_log: begin tx")
		return
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO commit_log (commit_hash, path, committed_at, message, operation, author_email, action) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		log.Warn().Err(err).Msg("commit_log: prepare")
		return
	}
	defer stmt.Close()

	for _, f := range files {
		if _, err := stmt.Exec(hashStr, f.path, ts, msg, op, authorEmail, f.action); err != nil {
			log.Warn().Err(err).Str("path", f.path).Msg("commit_log: insert")
		}
	}
	if err := tx.Commit(); err != nil {
		log.Warn().Err(err).Msg("commit_log: commit tx")
	}
}

// commitLogAge is used in read.go for SQL activity queries.
func commitLogAge(days int) int64 {
	return time.Now().Unix() - int64(days)*86400
}
