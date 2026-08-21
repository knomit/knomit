package knomitapi

// ServerKey derives the MCP-config `mcpServers` key a host writes for a knomit
// server. Claude Code turns it into the tool-name prefix
// `mcp__<key>__knomit_learn`; Antigravity uses it only as an identifier.
//
// It is DERIVED rather than the constant "knomit" because the constant made
// scaffolding structurally single-server: a second init in the same project
// collided on the key, so two knomit servers could never coexist. A lens
// scoping wins over a repo scoping because the two are mutually exclusive at
// the flag layer and the lens is the thing actually being served.
//
// The prefix names the AXIS as well as the product, and is applied
// UNCONDITIONALLY, so the mapping from scope to key is injective: distinct
// scopes can never collide. Both halves are load-bearing.
//
// Naming the axis is what makes the two namespaces disjoint. Repos and lenses
// are separate namespaces validated by the same repos.IsValidName, so nothing
// stops a lens and a repo sharing a name, and a shared `knomit-` prefix would
// map `--repo eng` and `--lens eng` to the same key. Since `knomit-repo-` and
// `knomit-lens-` are fixed-length and differ, no repo key can ever equal a lens
// key, whatever the names. See TestServerKey_IsInjective.
//
// Applying it unconditionally is the other half. An earlier draft skipped the
// prefix when the name already carried it, to avoid the ugly `knomit-knomit`
// for a repo named `knomit` — but that rule is inherently many-to-one (`web`
// and `knomit-web` both map to `knomit-web`), which re-creates in one step
// exactly the clobbering this function exists to remove.
//
// Callers must validate name/lens with repos.IsValidName first: the result is
// interpolated into JSON, and this function does no escaping of its own.
func ServerKey(repoName, lens string) string {
	if lens != "" {
		return "knomit-lens-" + lens
	}
	return "knomit-repo-" + repoName
}

// MaxServerKeyLen bounds the derived key so the fully-qualified tool name
// Claude Code builds from it stays under the API's 64-character tool-name
// limit. The longest tool is knomit_hypothesize, giving
// len("mcp__") + len(key) + len("__") + len("knomit_hypothesize") = 25 + key.
//
// Bytes, not runes: repos.IsValidName restricts names to ASCII, so the two
// counts coincide and the byte length is what the API actually measures.
//
// Antigravity does not need this bound — it exposes bare tool names — but a
// repo scaffolded for both hosts must satisfy the stricter rule anyway, and one
// conservative rule beats two divergent ones.
const MaxServerKeyLen = 64 - len("mcp____knomit_hypothesize")

// MaxScopeNameLen is the resulting budget for the repo or lens NAME itself —
// what an error message must quote, since that is the knob the user turns.
// Both axis prefixes are the same length, so one constant covers both.
const MaxScopeNameLen = MaxServerKeyLen - len("knomit-repo-")
