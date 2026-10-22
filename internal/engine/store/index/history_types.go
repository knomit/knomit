package index

import (
	"errors"
)

type LogEntry struct {
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Message string `json:"message"`
}

type FileCounts struct {
	Added    int `json:"added,omitempty"`
	Modified int `json:"modified,omitempty"`
	Deleted  int `json:"deleted,omitempty"`
}

// CommitAuthor is the git author identity of a commit, recorded verbatim:
// Name is the agent-id (or human name on a merge); Email carries the
// +operation subaddress for agent writes. Distinct from the committer, which
// drops the operation tag (agents) or is GitHub itself (PR merges).
type CommitAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type LogEntryWithTags struct {
	Commit    string       `json:"commit"`
	Date      string       `json:"date"`
	Message   string       `json:"message"`
	Operation string       `json:"operation,omitempty"`
	Author    CommitAuthor `json:"author"`
	Files     FileCounts   `json:"files,omitempty"`
}

type ChangedFile struct {
	Path   string `json:"path"`
	Action string `json:"action"` // "added", "modified", "deleted"
}

type CommitDetailResult struct {
	Commit    string        `json:"commit"`
	Date      string        `json:"date"`
	Message   string        `json:"message"`
	Operation string        `json:"operation,omitempty"`
	Author    CommitAuthor  `json:"author"`
	Files     []ChangedFile `json:"files"`
}

type ActivityResult struct {
	LastCommit string `json:"last_commit"` // ISO-8601 timestamp of most recent commit, or ""
	Total      int    `json:"total"`       // total commits touching this path
	Changes7d  int    `json:"changes_7d"`
	Changes30d int    `json:"changes_30d"`
	Changes90d int    `json:"changes_90d"`
}

// ErrFactNotLive is returned by ExplainFact (and any other HEAD-anchored read
// path that needs to resolve a path's active commit via branch_facts) when
// no row exists for (branch, path). This means the fact is not currently live
// at HEAD — typically because it was retracted, or because the path was never
// indexed on this branch. Older versions may still exist in the graph and be
// reachable via commit-anchored endpoints.
var ErrFactNotLive = errors.New("fact not live at HEAD")
