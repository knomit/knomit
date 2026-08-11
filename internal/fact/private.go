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

// PrivateRoot is the namespace knomit owns inside a KB repo: everything
// knomit-owned that is not knowledge lives under it.
//
// The rule it encodes: knomit-owned private data lives under .knomit/;
// anything a git provider resolves by exact name (README.md, LICENSE,
// .github/) stays at the tree root; every other dot-root is FOREIGN and
// knomit never writes to it.
const PrivateRoot = ".knomit"

// reservedPrivate names server-owned SUBTREES inside PrivateRoot. Loose files
// at the namespace root are already protected by the depth rule below, so this
// list only earns its keep when a server-owned DIRECTORY appears.
//
// Empty is the correct state today, not an oversight.
var reservedPrivate = []string{}

// IsWritablePrivatePath reports whether knomit's fact tools may write path.
//
// Agent-writable = PrivateRoot/<area>/… — at least one subdirectory deep —
// minus reservedPrivate. Loose files at the root of .knomit/ are knomit's own
// config; subdirectories belong to agents.
//
// The asymmetry is deliberate: a new SERVER-owned path arrives with the code
// that writes it, so reserving it costs one line in the same change. A new
// AGENT-owned area arrives at runtime, from a job that ships no code, and must
// work without a release. So the thing needing no code change is exactly the
// thing that has no code.
//
// This is an ALLOW-LIST rooted in ownership, deliberately not a deny-list of
// foreign dot-roots. A deny-list fails OPEN: the moment a KB repo picks up
// .vscode/, .idea/ or .devcontainer/, knomit would be authorized to write
// another tool's territory. Rooting the rule in ownership fails CLOSED.
//
// It does NOT weaken IsPrivatePath. A writable private path is still private:
// excluded from discovery at every walker, which is the entire point — job
// state wants to be invisible to readers while remaining writable by its job.
func IsWritablePrivatePath(path string) bool {
	rest, ok := strings.CutPrefix(path, PrivateRoot+"/")
	if !ok || !strings.Contains(rest, "/") {
		return false
	}
	for _, r := range reservedPrivate {
		if rest == r || strings.HasPrefix(rest, r+"/") {
			return false
		}
	}
	return true
}
