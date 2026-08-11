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

// PrivateRoot is the namespace knomit owns inside a KB repo: everything
// knomit-owned that is not knowledge lives under it.
//
// The rule it encodes: knomit-owned private data lives under .knomit/;
// anything a git provider resolves by exact name (README.md, LICENSE,
// .github/) stays at the tree root; every other dot-root is FOREIGN and
// knomit never writes to it.
const PrivateRoot = ".knomit"

// OntologyFile is the ontology definition's path inside PrivateRoot, and
// LegacyOntologyFile is where it lived before the namespace was consolidated.
//
// They live in `fact` rather than `repos` because BOTH repos and okf/source
// need them, and neither imports the other — okf/source previously carried
// its own duplicated copies of these literals, which is exactly the drift
// this placement prevents.
//
// A loose file at the ROOT of the namespace is what makes the ontology
// server-owned: IsWritablePrivatePath requires at least one subdirectory AND a
// dotless <area>, so no agent can rewrite it through the fact tools — neither
// by naming it directly nor by reusing its name as a directory.
//
// Every server-owned loose file added here MUST therefore carry a dot in its
// name. One that does not (".knomit/manifest") is shadowable by an area of the
// same name and needs a reservedPrivate entry instead; TestServerOwnedLooseFilesAreDotted
// is what fails if that premise is ever broken.
const (
	OntologyFile       = PrivateRoot + "/ontology.yaml"
	LegacyOntologyFile = ".domains/ontology.yaml"
)

// reservedPrivate names server-owned SUBTREES inside PrivateRoot. Loose files
// at the namespace root are already protected by the depth and dotless-area
// rules below — by name AND against their name being reused as a directory —
// so this list only earns its keep when a server-owned DIRECTORY appears, or
// when a server-owned loose file is given a dotless name.
//
// Empty is the correct state today, not an oversight.
var reservedPrivate = []string{}

// IsWritablePrivatePath reports whether knomit's fact tools may write path.
//
// Agent-writable = PrivateRoot/<area>/… — at least one subdirectory deep, with
// <area> a DOTLESS directory name — minus reservedPrivate. Loose files at the
// root of .knomit/ are knomit's own config; subdirectories belong to agents.
//
// The area must contain no "." because depth alone protects a server-owned
// loose file by NAME only, not against that name being reused as a DIRECTORY.
// store's buildTree replaces an existing entry of the same name whatever its
// mode, so ".knomit/ontology.yaml/x.md" turns the ontology BLOB into a tree:
// every later read fails, loadOntology falls through to the embedded default,
// and every subsequent fact is validated against the wrong taxonomy with only
// a log line to show for it. Areas are plain directory names (runs, crawlers),
// so a dotless rule costs legitimate callers nothing and covers every current
// AND future server-owned loose file with no list to maintain — the same
// principle as the asymmetry below: the thing needing no code change is the
// thing that has no code.
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
//
// The path is judged AS GIVEN, so it must be self-sufficient: "..", "." and
// empty segments are rejected outright rather than trusted to be normalized
// away. Each of them defeats the rule this function exists to enforce —
// ".knomit/a/../../kb/x.md" leaves the namespace entirely, ".knomit/./x.md"
// and ".knomit//x.md" name a loose file at the namespace root. This is an
// AUTHORIZATION predicate: its answer must not depend on a check in a package
// it never references, or the first caller that asks the question without
// going through the store gets a yes it should never have had.
//
// ".." is rejected as a SUBSTRING, not just as a whole segment, because that
// is the rule store.validatePath enforces and batchWrite pre-flights every
// path through it. Authorizing "..hidden" would hand the caller a yes and then
// fail at write — and learn is all-or-nothing, so the doomed path takes the
// whole batch down with an error naming the wrong cause. A predicate that
// authorizes what the writer refuses is a trap; the two must agree.
func IsWritablePrivatePath(path string) bool {
	rest, ok := strings.CutPrefix(path, PrivateRoot+"/")
	if !ok {
		return false
	}
	if strings.Contains(rest, "..") {
		return false
	}
	segs := strings.Split(rest, "/")
	// At least one subdirectory deep: <area>/<name>.
	if len(segs) < 2 {
		return false
	}
	// <area> is a plain directory name: no dot, so it can never collide with a
	// server-owned loose file at the namespace root.
	if strings.Contains(segs[0], ".") {
		return false
	}
	for _, seg := range segs {
		if seg == "" || seg == "." {
			return false
		}
	}
	for _, r := range reservedPrivate {
		if rest == r || strings.HasPrefix(rest, r+"/") {
			return false
		}
	}
	return true
}
