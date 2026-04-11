package hal

import "strings"

// Anchor captures the temporal context for a fact URL: which branch and,
// optionally, which commit. An empty Commit means "branch HEAD"; a set
// Commit means "the fact as it existed at this commit". All URL construction
// for the v2 API goes through an Anchor so the commit-anchored rule is
// enforced by construction: links built from a commit-anchored Anchor carry
// the /commits/{sha}/ segment.
type Anchor struct {
	Branch string // canonical name, slashes preserved
	Commit string // empty means HEAD
}

// IsHEAD reports whether this anchor targets the branch HEAD (no specific
// commit pin).
func (a Anchor) IsHEAD() bool { return a.Commit == "" }

// EncodeBranch substitutes `/` with `:` for use in URL path segments.
// Branches like "agent/test" become "agent:test" in URLs.
func EncodeBranch(name string) string {
	return strings.ReplaceAll(name, "/", ":")
}

// DecodeBranch reverses EncodeBranch.
func DecodeBranch(urlSegment string) string {
	return strings.ReplaceAll(urlSegment, ":", "/")
}
