package fact

import "strings"

// IsPrivatePath reports whether any segment of path begins with ".".
//
// A dot-prefixed directory or file is MACHINERY, not knowledge: .github/ holds
// CI config, .domains/ holds the ontology definition. Private paths are
// excluded from fact DISCOVERY everywhere — the search indexer, Verify, and the
// OKF exporter all skip them, and the fact-creation paths refuse to allocate
// one.
//
// Private means "not knowledge content", NOT "invisible to knomit". knomit
// still reads specific known paths by name, which is precisely what lets it
// load .domains/ontology.yaml. The rule governs walking, never opening.
//
// This is a LOCATION test, so it extends the ontology-root scope rule rather
// than competing with it: membership is decided by where a file sits, never by
// whether its bytes happen to parse.
func IsPrivatePath(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}
