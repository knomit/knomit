package hal

import "strings"

// Anchor captures the temporal context for a fact URL: which branch and,
// optionally, which commit. An empty Commit means "branch HEAD"; a set
// Commit means "the fact as it existed at this commit". All URL construction
// for the API goes through an Anchor so the commit-anchored rule is
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

// URLBuilder constructs API URLs. All URL construction in the API router
// goes through this type so the branch-name substitution and the
// commit-anchor propagation are enforced in exactly one place.
//
// Base is the prefix under which the API router is mounted (e.g. "/api/v1").
type URLBuilder struct {
	Base string
}

// APIRoot returns the root URL (links to /repos, /openapi.yaml).
func (b URLBuilder) APIRoot() string { return b.Base }

// Repos returns the repo collection URL.
func (b URLBuilder) Repos() string { return b.Base + "/repos" }

// Repo returns a single repo resource URL.
func (b URLBuilder) Repo(repo string) string { return b.Base + "/repos/" + repo }

// Branches returns the branch collection URL for a repo.
func (b URLBuilder) Branches(repo string) string {
	return b.Repo(repo) + "/branches"
}

// Branch returns a branch resource URL, encoding the branch name.
func (b URLBuilder) Branch(repo string, a Anchor) string {
	return b.Branches(repo) + "/" + EncodeBranch(a.Branch)
}

// BranchOrCommitPrefix returns the path prefix under which a resource lives,
// respecting the anchor's commit pin if any.
func (b URLBuilder) BranchOrCommitPrefix(repo string, a Anchor) string {
	p := b.Branch(repo, a)
	if !a.IsHEAD() {
		p += "/commits/" + a.Commit
	}
	return p
}

// Fact returns the URL for a single fact at the given anchor. Path is the
// git-relative fact path (e.g. "know/ai/ml/abc12345.md").
func (b URLBuilder) Fact(repo string, a Anchor, path string) string {
	return b.BranchOrCommitPrefix(repo, a) + "/facts/" + path
}

// FactIncoming returns the URL for a fact's incoming-edges collection.
// Valid on both HEAD-anchored URIs (returns the lineage of the current
// version) and commit-anchored URIs (returns the lineage of that specific
// version).
func (b URLBuilder) FactIncoming(repo string, a Anchor, path string) string {
	return b.Fact(repo, a, path) + "/incoming"
}

// FactOutgoing returns the URL for a fact's outgoing-edges collection.
func (b URLBuilder) FactOutgoing(repo string, a Anchor, path string) string {
	return b.Fact(repo, a, path) + "/outgoing"
}

// Lenses returns the lens collection URL.
func (b URLBuilder) Lenses() string { return b.Base + "/lenses" }

// Lens returns a single lens resource URL.
func (b URLBuilder) Lens(name string) string { return b.Base + "/lenses/" + name }

// Archived returns the URL of the archived-repos collection.
func (b URLBuilder) Archived() string { return b.Base + "/archived" }

// ArchivedItem returns the URL of a single archived repo by id.
func (b URLBuilder) ArchivedItem(id string) string { return b.Base + "/archived/" + id }

// FactCommits returns the URL for a fact's per-fact commit log. Always
// branch-anchored (commit logs are not themselves commit-pinned).
func (b URLBuilder) FactCommits(repo string, a Anchor, path string) string {
	head := Anchor{Branch: a.Branch} // strip commit pin
	return b.Fact(repo, head, path) + "/commits"
}
