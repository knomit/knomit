package fact

import (
	"errors"
	"strings"
)

// ErrTitleRequired is the refusal for a fact whose title is absent, empty or
// whitespace-only.
var ErrTitleRequired = errors.New("title is required")

// ValidateTitle refuses a title with nothing but whitespace in it. Such a fact
// serialises as a bare "# " heading, which ParseFact refuses with "empty title
// heading" — so the write would commit a fact that every reader (query,
// explain, update, the indexer) then rejects, repairable only by retract and
// relearn.
//
// SerializeFact calls it, and every write path passes through SerializeFact,
// so this one rule is the gate for all of them. A handler may also call it
// early to report the refusal in its own terms (knomit_learn names the fact
// index, the REST create handler answers 400).
func ValidateTitle(title string) error {
	if strings.TrimSpace(title) == "" {
		return ErrTitleRequired
	}
	return nil
}
