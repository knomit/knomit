// commit_log population and append helpers.
// The commit_log table is a denormalized index of (commit_hash, path, committed_at, message)
// that enables O(1) activity aggregates and efficient path-history queries.
//
// Queries use commit_hash as a tiebreaker when commits share the same committed_at timestamp,
// giving deterministic and stable pagination without depending on insertion order.
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

	storegit "knomit/internal/store/git"
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

// commitEntries converts changedFile results from a commit into CommitLogEntry structs.
func commitEntries(c *object.Commit, files []changedFile) []storegit.CommitLogEntry {
	hashStr := c.Hash.String()
	ts := c.Committer.When.Unix()
	msg := firstLine(c.Message)
	authorEmail := c.Author.Email
	op := parseOperation(authorEmail)

	entries := make([]storegit.CommitLogEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, storegit.CommitLogEntry{
			Hash:        hashStr,
			Path:        f.path,
			Message:     msg,
			Operation:   op,
			AuthorEmail: authorEmail,
			Action:      f.action,
			CommittedAt: ts,
		})
	}
	return entries
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
			files = append(files, changedFile{path: strings.ToLower(path), action: action})
		}
	}
	return files, nil
}

// populateCommitLog backfills commit_log from the tip of branch.
// Commits are streamed directly from the iterator to CommitLogSync, which stops
// as soon as it encounters a hash already in the table (dedup / incremental update).
func (s *Store) populateCommitLog(branch string) error {
	hash, err := s.resolveRef(branch)
	if err != nil {
		// Branch not found (empty repo) — just mark available if table exists.
		_ = s.storer.CommitLogAvailable()
		return nil
	}

	logIter, err := s.repo.Log(&gogit.LogOptions{
		From:  hash,
		Order: gogit.LogOrderDefault,
	})
	if err != nil {
		return fmt.Errorf("populateCommitLog: log: %w", err)
	}
	defer logIter.Close()

	var count int
	err = s.storer.CommitLogSync(func() (string, []storegit.CommitLogEntry, error) {
		c, err := logIter.Next()
		if err == io.EOF {
			return "", nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		count++
		files, err := changedFilesInCommit(c)
		if err != nil {
			return "", nil, err
		}
		return c.Hash.String(), commitEntries(c, files), nil
	})
	if err != nil {
		return fmt.Errorf("populateCommitLog: sync: %w", err)
	}

	log.Debug().Int("commits", count).Msg("commit_log: populated")
	return nil
}

// appendCommitLog inserts a single new commit into commit_log.
// New commits always get the highest rowid, preserving recency ordering.
// Errors are logged and swallowed — commit_log is an index, not source of truth.
func (s *Store) appendCommitLog(hash plumbing.Hash) {
	if !s.storer.CommitLogAvailable() {
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
	done := false
	entries := commitEntries(c, files)
	if err := s.storer.CommitLogSync(func() (string, []storegit.CommitLogEntry, error) {
		if done {
			return "", nil, nil
		}
		done = true
		return hash.String(), entries, nil
	}); err != nil {
		log.Warn().Err(err).Str("hash", hash.String()[:8]).Msg("commit_log: append sync failed")
	}
}

// commitLogAge is used in read.go for SQL activity queries.
func commitLogAge(days int) int64 {
	return time.Now().Unix() - int64(days)*86400
}
