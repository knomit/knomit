package index

import (
	"knomit/internal/fact/textnorm"
)

// The three normalizers below moved to internal/fact/textnorm so internal/fact
// can use the SAME definition of "the same token" for the motif subject-word
// strip without inverting the store -> fact dependency (internal/fact imports
// no internal package, by design). Two stemmers are two things that drift, and
// a drifted stemmer lets a motif that renames its own fact's subject past the
// strip.
//
// They stay as unexported names here because every call site in this package
// reads better with the domain-specific name, and because renaming ~30 call
// sites would bury a mechanical extraction inside an unrelated diff. The
// existing domain-match suite is the zero-diff gate: no test was edited.
func canonicalizeDomain(s string) string { return textnorm.Canonicalize(s) }

func stemDomainToken(t string) string { return textnorm.Stem(t) }

func domainTokens(canonical string) []string { return textnorm.Tokens(canonical) }
