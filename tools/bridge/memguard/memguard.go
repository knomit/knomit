// Package memguard decides whether a tool call would write a team-relevant
// note into a Claude Code auto-memory directory, where no other agent can see
// it.
//
// It lives in its own package because both agent hosts need the identical
// answer: the Claude Code PreToolUse hook denies the call outright, and the
// Antigravity pre-invocation hook can only warn, but a rule the two hosts
// disagree about is worse than either alone.
package memguard

import (
	"path"
	"regexp"
	"strings"
)

// Decision is the classifier's answer. Allowed is the default for everything
// this package does not positively recognise as a violation: a guard that
// blocks unrelated work would be turned off, and then it protects nothing.
type Decision struct {
	Deny   bool
	Reason string
}

// memoryPathRe matches a file inside Claude Code's per-project auto-memory
// directory, e.g. ~/.claude/projects/-home-me-proj/memory/note.md. The project
// segment is whatever the host derived from the working directory, so a
// worktree spells it differently (…-proj-worker) and must match too.
var memoryPathRe = regexp.MustCompile(`(^|/)\.claude/projects/[^/]+/memory/`)

// writeOperatorRe matches a shell WRITE, as a token. Matching words instead of
// operators is the mistake that makes this guard useless: a prototype matched
// "echo" and blocked `echo $(cat memory/MEMORY.md)`, a plain read. `echo` and
// `printf` only write when a redirection operator is also present, and the
// operator is what this matches.
var writeOperatorRe = regexp.MustCompile(`(>>?|\|\s*tee\b|\btee\b|\bsed\b[^|;&]*-i|\brm\b|\bmv\b|\bcp\b|\bdd\b|\binstall\b|\btruncate\b|\bpython[0-9.]*\b[^|;&]*-c|\bperl\b[^|;&]*-e|\bruby\b[^|;&]*-e|\bnode\b[^|;&]*-e)`)

// Reason is the text both hosts show. It says what is wrong, where the note
// should go instead, and the one exception — because a deny with no route
// forward gets worked around rather than followed.
const Reason = "This path is Claude Code's PRIVATE auto-memory directory: it belongs to one session on one machine and is invisible to every other agent, so a note written here is lost to the team. " +
	"Anything another agent could use — a lesson, a gotcha, a convention, an environment limit, a decision — belongs in knomit: use /knomit-remember, /knomit-decided, or /knomit-update. " +
	"Only `type: user` memories (facts about the person) and MEMORY.md itself may be written here."

// CheckFileWrite classifies a Write/Edit/MultiEdit against filePath.
//
// frontmatter is the content the write would produce (Write) or the current
// contents of the file on disk (Edit/MultiEdit) — whichever is available. An
// empty string means the caller could not read it, and the path is judged on
// its own.
func CheckFileWrite(filePath, frontmatter string) Decision {
	if filePath == "" || !memoryPathRe.MatchString(filePath) {
		return Decision{}
	}
	// MEMORY.md is the index the host itself maintains; it is not a note.
	if path.Base(filePath) == "MEMORY.md" {
		return Decision{}
	}
	if isUserType(frontmatter) {
		return Decision{}
	}
	return Decision{Deny: true, Reason: Reason}
}

// CheckBash classifies a Bash command. It denies only when the command both
// references an auto-memory directory AND carries a write operator, so reading
// the directory — which is legitimate and common — stays allowed.
func CheckBash(command string) Decision {
	if command == "" {
		return Decision{}
	}
	if !strings.Contains(command, ".claude/projects/") || !strings.Contains(command, "memory") {
		return Decision{}
	}
	if !writeOperatorRe.MatchString(command) {
		return Decision{}
	}
	return Decision{Deny: true, Reason: Reason}
}

// isUserType reports whether YAML frontmatter declares `type: user`, the one
// memory kind that legitimately belongs to a single person rather than the team.
//
// It scans only the leading frontmatter block: a `type: user` line further down
// a document body is prose, not a declaration.
func isUserType(content string) bool {
	s := strings.TrimLeft(content, " \t\r\n")
	if !strings.HasPrefix(s, "---") {
		return false
	}
	s = s[3:]
	end := strings.Index(s, "\n---")
	if end < 0 {
		return false
	}
	for _, line := range strings.Split(s[:end], "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 2 && f[0] == "type:" && f[1] == "user" {
			return true
		}
	}
	return false
}
