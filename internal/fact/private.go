package fact

import "strings"

// IsPrivatePath reports whether any segment of path begins with ".".
//
// A dot-prefixed directory or file is MACHINERY, not knowledge: .github/ holds
// CI config, .knomit/ holds the ontology definition. Private paths are
// excluded from fact DISCOVERY everywhere — the search indexer, Verify, and the
// OKF exporter all skip them, and the fact-creation paths refuse to allocate
// one.
//
// Private means "not knowledge content", NOT "invisible to knomit". knomit
// still reads specific known paths by name, which is precisely what lets it
// load the ontology file out of PrivateRoot. The rule governs walking, never
// opening.
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

// IsSystemFilePath reports whether p names a file under PrivateRoot that the
// fact tools may READ by its exact path: ".knomit/" followed by at least one
// segment, with no empty segment (so no "//" and no trailing "/") and no "."
// or ".." segment. Dot-prefixed segments below the root are allowed —
// ".knomit/templates/mission/.knomit/ontology.yaml" is a real template file.
//
// This is a READ rule only. Every write to PrivateRoot stays refused; a path
// that passes here is not thereby writable. Case is significant: ".KNOMIT/x"
// is not a system path.
func IsSystemFilePath(p string) bool {
	rest, ok := strings.CutPrefix(p, PrivateRoot+"/")
	if !ok || rest == "" {
		return false
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// PrivateRoot is the SYSTEM root of a KB repo: the ontology, trigger scripts,
// recipes, skills — what describes how the knowledge base operates, the way
// the system tables describe a database.
//
// Its writers are people, through git (a commit, a push, a merge), and
// knomit's own code (creating a repo, the boot-time preset refresh, creating
// from a template). It is CLOSED to every fact-tool WRITE — knomit_learn
// (path and retract), knomit_update, knomit_retract, the REST fact PUT/DELETE
// and experiment resolutions — exactly like every other dot path (F25, user
// ruling 2026-10-05). Agents' own working files live under ArtifactsRoot.
//
// It is OPEN to READS by exact path (user ruling 2026-10-06): knomit_explain
// and the REST fact GET return a file under it raw (IsSystemFilePath), and a
// fact may ref one. It is still never indexed, queried or listed, and every
// OTHER dot path stays closed to reads as well as writes.
//
// Anything a git provider resolves by exact name (README.md, LICENSE,
// .github/) stays at the tree root; every other dot-root is FOREIGN and
// knomit never writes to it.
const PrivateRoot = ".knomit"

// OntologyFile is the ontology definition's path inside PrivateRoot;
// LegacyOntologyFile and PreDotOntologyFile are where it lived before, newest
// first.
//
// All three rungs are READ, none is written by a migration — repos are moved
// by hand. The oldest one matters most: .domains/ was introduced only six days
// before .knomit/, so a repo that has not been hand-migrated is far likelier to
// hold domains/ontology.yaml than .domains/ontology.yaml. Dropping that rung
// silently drops such a repo onto DefaultOntology() behind one log.Warn, and
// every fact written afterwards is validated against the wrong taxonomy with
// nothing tying the bad facts back to the cause. Read rungs are cheap; a wrong
// taxonomy is not.
//
// They live in `fact` rather than `repos` because BOTH repos and okf/source
// need them, and neither imports the other — okf/source previously carried
// its own duplicated copies of these literals, which is exactly the drift
// this placement prevents.
//
// No agent can rewrite the ontology through the fact tools — neither by
// naming it nor by reusing its name as a directory
// (".knomit/ontology.yaml/x.md"): every path under PrivateRoot is a dot path,
// and the fact tools refuse every dot path.
const (
	OntologyFile       = PrivateRoot + "/ontology.yaml"
	LegacyOntologyFile = ".domains/ontology.yaml"
	PreDotOntologyFile = "domains/ontology.yaml"
)

// OntologyPathsNewestFirst is the read order every ontology reader walks:
// canonical first, then each legacy location oldest-last. A reader stops at
// the first rung that yields content, and a writer that refreshes what it read
// writes BACK to that same rung — writing the canonical path instead leaves an
// unmigrated repo holding two ontology files with nothing to distinguish the
// live one from the stale one.
//
// Returned as a fresh slice rather than exported as a package-level var so a
// caller cannot reorder or truncate the chain for everyone else.
func OntologyPathsNewestFirst() []string {
	return []string{OntologyFile, LegacyOntologyFile, PreDotOntologyFile}
}
