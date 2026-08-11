package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"

	storegit "knomit/internal/store/git"
)

// DiffFiles returns paths added/modified/deleted between fromCommit and the tip of branch.
// Only .md files are returned. If fromCommit is empty, diffs from empty tree.
func (fi *factIndex) DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error) {
	return fi.rh.DiffFiles(ctx, branch, fromCommit)
}

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

// changedFileEntry represents a file changed in a commit (internal to commitlog).
type changedFileEntry struct {
	path   string
	action string // "added", "modified", "deleted"
}

// commitEntries converts changedFileEntry results from a commit into CommitLogEntry structs.
func commitEntries(c *object.Commit, files []changedFileEntry) []storegit.CommitLogEntry {
	hashStr := c.Hash.String()
	ts := c.Committer.When.Unix()
	msg := firstLine(c.Message)
	authorName := c.Author.Name
	authorEmail := c.Author.Email
	op := parseOperation(authorEmail)

	entries := make([]storegit.CommitLogEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, storegit.CommitLogEntry{
			Hash:        hashStr,
			Path:        f.path,
			Message:     msg,
			Operation:   op,
			AuthorName:  authorName,
			AuthorEmail: authorEmail,
			Action:      f.action,
			CommittedAt: ts,
		})
	}
	return entries
}

// changedFilesInCommit returns every file added/modified/deleted in c.
// Historically this filtered to .md files only, but that left commit_log
// sparse for any commit that touched non-.md files (e.g. the InitRepo
// commit that seeds the ontology file). The Verify tool's commit-log
// parity check (Task 1.3) requires every reachable commit to have at least
// one commit_log row, so the filter was removed in 2026-04-08. Callers
// that need a .md-only view must filter themselves.
func changedFilesInCommit(c *object.Commit) ([]changedFileEntry, error) {
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
	var files []changedFileEntry
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
		files = append(files, changedFileEntry{path: strings.ToLower(path), action: action})
	}
	return files, nil
}

// commitLogAge returns a Unix timestamp for `days` days ago.
func commitLogAge(days int) int64 {
	return time.Now().Unix() - int64(days)*86400
}
