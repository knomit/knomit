// Fact markdown parsing: converts a knomit fact file (YAML frontmatter +
// markdown body) into a FactRecord. Used during git sync to index new or
// modified files.
package store

import (
	"knomit/internal/fact"
)

// parseFact parses a knomit fact markdown file into a FactRecord.
// Expected format:
//
//	---
//	domain: [databases, sql]
//	confidence: 0.9
//	sources: 2
//	entities: [postgres, mysql]
//	refs: []
//	---
//	# Title of the fact
//
//	Body content.
func parseFact(path, content string) (FactRecord, error) {
	f, err := fact.ParseFact(path, content)
	if err != nil {
		return FactRecord{}, err
	}
	return FactRecord{
		Path:           f.Path(),
		Title:          f.Title,
		Type:           string(f.Type),
		Domain:         f.Domain,
		Entities:       f.Entities,
		Confidence:     f.Confidence,
		Sources:        f.Sources,
		Refs:           f.Refs,
		EvidenceWeight: f.EvidenceWeight,
	}, nil
}
