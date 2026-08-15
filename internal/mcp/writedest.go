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
// Lens shape note: `repo` is always the WRITE repo, never the lens. A lens reads
// a union but writes to exactly one member, and reporting the lens name as the
// destination would misdescribe where the bytes are — the lens is reported
// alongside, as context for why that repo was chosen.
type writeDestination struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Lens   string `json:"lens,omitempty"`
}

// describeWriteDestination reads the destination off the binding. Safe to call
// only after the handler's WriteOK gate, which is where every write tool
// resolves its binding.
func describeWriteDestination(b *repos.Binding) writeDestination {
	ri := b.Write()
	d := writeDestination{Repo: ri.Name(), Branch: ri.AgentBranch()}
	if b.IsLens() {
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
	if d.Lens != "" {
		return fmt.Sprintf("wrote %s to repo %q on branch %q — the write repo of lens %q",
			what, d.Repo, d.Branch, d.Lens)
	}
	return fmt.Sprintf("wrote %s to repo %q on branch %q", what, d.Repo, d.Branch)
}

// pluralFacts renders a fact count for the summary sentence.
func pluralFacts(n int) string {
	if n == 1 {
		return "1 fact"
	}
	return fmt.Sprintf("%d facts", n)
}
