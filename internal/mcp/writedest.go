package mcp

import (
	"fmt"

	"knomit/internal/repos"
)

// writeDestination describes where a write actually landed, so the caller can
// SEE it rather than infer it.
//
// Why this exists: the write destination is a property of the connection, fixed
// at process start and resolved from the request context — it is deliberately
// not expressible in model output (a `repo:` parameter would relocate the choice
// into tool arguments, which the corpus rejects on two separate grounds). That
// is the right design, but it has a cost: the caller has no way to confirm where
// a fact went, and a fact written to an unintended repo returns success exactly
// like one written to the intended repo. Measurement across two connected knomit
// servers never produced a wrong-server write, so this is not a fix for an
// observed bug — it is what makes such a write recoverable instead of silent if
// one ever happens, and what lets a caller correct itself in the same turn.
//
// Identity note: `repo` is the display NAME and `repo_id` is the IDENTITY, and
// the stamp needs both. A repo has three identifiers with different lifetimes,
// and the name is the one that is neither stable nor globally unique — it is
// per-machine, mutable, and unique only among the ACTIVE repos of ONE server.
// Two connected knomit servers each holding a repo called "knomit" on
// "agent/main" is the ordinary case, and it is exactly the case this stamp
// exists to disambiguate: with the name alone, a write to the wrong server
// renders byte-identically to a write to the right one and the stamp detects
// nothing. RepoID is the 12-hex root-commit form (RepoInstance.ShortID) —
// clone-stable, rename-safe, and the same identifier that already appears in
// kb://<id>/… paths and the knomit_repos mount table. The name stays because
// prose is what a caller reads; the id is what makes the prose discriminating,
// and what keeps the stamp's meaning fixed across a rename.
//
// Lens shape note: `repo` is always the WRITE repo, never the lens. A lens reads
// a union but writes to exactly one member, and reporting the lens name as the
// destination would misdescribe where the bytes are — the lens is reported
// alongside, as context for why that repo was chosen.
type writeDestination struct {
	Repo   string `json:"repo"`
	RepoID string `json:"repo_id,omitempty"`
	Branch string `json:"branch"`
	Lens   string `json:"lens,omitempty"`
}

// describeWriteDestination reads the destination off the binding. Safe to call
// only after the handler's WriteOK gate, which is where every write tool
// resolves its binding.
//
// RepoID is omitempty because ShortID returns "" when the store is unavailable
// and identity is genuinely unknown — an empty string there is honest, and
// better than a field the caller would read as an id.
func describeWriteDestination(b *repos.Binding) writeDestination {
	ri := b.Write()
	d := writeDestination{Repo: ri.Name(), RepoID: ri.ShortID(), Branch: ri.AgentBranch()}
	// FromLens, not IsLens. IsLens asks about federation BREADTH — whether the
	// binding reads more than its own write repo — which a lens mounting a
	// single member does not. But that caller still connected THROUGH a lens,
	// and which connection a write went through is the one thing this stamp
	// exists to let them confirm; dropping the lens there makes the result
	// indistinguishable from a direct /repos/{repo}/… connection.
	if b.FromLens() {
		d.Lens = b.Name()
	}
	return d
}

// summary renders the destination as a sentence.
//
// The structured field alone is not enough: what a model reacts to is prose in
// the result, and the whole point of stamping the destination is that a caller
// NOTICES an unintended one. Kept to one line so it cannot crowd the result.
func (d writeDestination) summary(what string) string {
	// The id rides in the prose, not only in the struct: a caller comparing two
	// servers reads the sentence, and a sentence that names only the display
	// name is the same sentence on both of them.
	where := fmt.Sprintf("repo %q", d.Repo)
	if d.RepoID != "" {
		where = fmt.Sprintf("repo %q (%s)", d.Repo, d.RepoID)
	}
	if d.Lens != "" {
		return fmt.Sprintf("wrote %s to %s on branch %q — the write repo of lens %q",
			what, where, d.Branch, d.Lens)
	}
	return fmt.Sprintf("wrote %s to %s on branch %q", what, where, d.Branch)
}

// pluralFacts renders a fact count for the summary sentence.
func pluralFacts(n int) string {
	if n == 1 {
		return "1 fact"
	}
	return fmt.Sprintf("%d facts", n)
}
