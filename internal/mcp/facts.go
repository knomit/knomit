package mcp

import "knomit/internal/fact"

// Fact is the in-memory representation of a knomit fact file.
// Defined in internal/fact; re-exported here for backward compatibility.
type Fact = fact.Fact

// ParseFact delegates to fact.ParseFact.
func ParseFact(path, content string) (Fact, error) { return fact.ParseFact(path, content) }

// SerializeFact delegates to fact.SerializeFact.
func SerializeFact(f Fact) string { return fact.SerializeFact(f) }
