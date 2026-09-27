package fact

import (
	"errors"
	"strings"
)

// ErrTitleRequired is the refusal for a fact whose title is absent, empty or
// whitespace-only.
var ErrTitleRequired = errors.New("title is required")

// ErrTitleMultiline is the refusal for a title carrying a line break.
var ErrTitleMultiline = errors.New("title must be a single line")

// ValidateTitle refuses a title the file format cannot store as written:
//
//   - nothing but whitespace: the fact serialises as a bare "# " heading,
//     which ParseFact refuses with "empty title heading", so the write would
//     commit a fact every reader (query, explain, update, the indexer) then
//     rejects, repairable only by retract and relearn;
//   - a line break (\n or \r) anywhere, edges included: "\nFoo" serialises as
//     that same empty heading, and "Foo\nBar" reads back as title "Foo" with
//     "Bar" moved into the body. It is checked on the raw input, before any
//     trimming, so a leading newline is refused rather than trimmed away.
//
// SerializeFact calls it, so it gates every write path that serialises a Fact
// — knomit_learn, knomit_update, REST create, experiment {body} resolutions,
// the synthesize pipelines and replay. REST PUT is the exception: it stores the
// client's bytes, and is protected by ParseFact, which it runs first and which
// refuses an empty heading (a multi-line title cannot reach it as a title: the
// heading is one line by construction). Handlers also call it early to report
// the refusal in their own terms (learn names the fact index, update names
// the field, REST create answers 400).
func ValidateTitle(title string) error {
	if strings.ContainsAny(title, "\r\n") {
		return ErrTitleMultiline
	}
	if strings.TrimSpace(title) == "" {
		return ErrTitleRequired
	}
	return nil
}
