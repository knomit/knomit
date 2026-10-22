package vcs

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"

	storegit "knomit/internal/engine/store/git"
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
func FirstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// ChangedFileEntry represents a file changed in a commit (internal to commitlog).
type ChangedFileEntry struct {
	Path   string
	Action string // "added", "modified", "deleted"
}

// commitEntries converts ChangedFileEntry results from a commit into CommitLogEntry structs.
func commitEntries(c *object.Commit, files []ChangedFileEntry) []storegit.CommitLogEntry {
	hashStr := c.Hash.String()
	ts := c.Committer.When.Unix()
	msg := FirstLine(c.Message)
	authorName := c.Author.Name
	authorEmail := c.Author.Email
	op := parseOperation(authorEmail)

	entries := make([]storegit.CommitLogEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, storegit.CommitLogEntry{
			Hash:        hashStr,
			Path:        f.Path,
			Message:     msg,
			Operation:   op,
			AuthorName:  authorName,
			AuthorEmail: authorEmail,
			Action:      f.Action,
			CommittedAt: ts,
		})
	}
	return entries
}

// ChangedFilesInCommit returns every file added/modified/deleted in c.
// Historically this filtered to .md files only, but that left commit_log
// sparse for any commit that touched non-.md files (e.g. the InitRepo
// commit that seeds the ontology file). The Verify tool's commit-log
// parity check (Task 1.3) requires every reachable commit to have at least
// one commit_log row, so the filter was removed in 2026-04-08. Callers
// that need a .md-only view must filter themselves.
func ChangedFilesInCommit(c *object.Commit) ([]ChangedFileEntry, error) {
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
	return changedFilesBetween(fromTree, toTree)
}

// changedFilesBetween returns every file added/modified/deleted from fromTree
// (nil: an empty tree) to toTree — ChangedFilesInCommit's diff, for callers
// that load the trees themselves.
func changedFilesBetween(fromTree, toTree *object.Tree) ([]ChangedFileEntry, error) {
	changes, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return nil, fmt.Errorf("changedFilesInCommit: diff: %w", err)
	}
	var files []ChangedFileEntry
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
		files = append(files, ChangedFileEntry{Path: strings.ToLower(path), Action: action})
	}
	return files, nil
}

// CommitLogAge returns a Unix timestamp for `days` days ago.
func CommitLogAge(days int) int64 {
	return time.Now().Unix() - int64(days)*86400
}
