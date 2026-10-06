package fact

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Guidance (F23) is repository policy for the synthesis prompts, declared in
// the ontology BY PATH and kept in files under .knomit/guidance/:
//
//	guidance:                       # root: applies to every topic
//	  hypothesize: guidance/all-hypothesize.md
//	topics:
//	  forecast:
//	    guidance:
//	      hypothesize: guidance/forecast-hypothesize.md
//	      review: guidance/forecast-review.md
//
// The ontology holds only paths, never the text (user ruling: no inline text).
// A value is relative to .knomit/, so `guidance/x.md` names
// `.knomit/guidance/x.md`. Knomit reads the ontology and the files at the tip
// of the consensus branch, at the same commit (repos.ConsensusGuidance) —
// never the agent branch or an experiment — and nothing in a fact, a task, a
// context value or a tool argument is ever substituted into the text.
//
// Every problem in a guidance block is a WARNING on the open path and fatal
// for a new ontology (newOnly), and a bad value resolves to NO file for that
// key at that node: it is not dropped in favour of the parent's path, so a
// typo fails closed instead of silently handing the topic another topic's
// policy.

// The guidance keys. The set is closed: a later key (a per-step review key)
// is unknown to this binary — a warning, preserved by Serialize, never read.
const (
	GuidanceHypothesize = "hypothesize"
	GuidanceReview      = "review"
)

// GuidanceKinds is the closed key set, in render order.
var guidanceKinds = []string{GuidanceHypothesize, GuidanceReview}

// GuidanceDir is the only directory a guidance path may name.
const GuidanceDir = PrivateRoot + "/guidance"

// MaxGuidanceBytes caps one guidance file, and separately the rendered
// guidance section of one work item.
const MaxGuidanceBytes = 16 << 10

// maxGuidancePathBytes bounds a path value. A path, not a paragraph.
const maxGuidancePathBytes = 256

// GuidanceBlock is a `guidance:` block on the ontology root or a topic node.
// Its UnmarshalYAML never fails, like TriggerList: a malformed block (a
// scalar, a list, inline text) must never make the ontology fatal on the open
// path, where a parse failure refuses every write. The raw node is kept so
// Serialize writes back exactly what was read, a newer knomit's keys included.
type GuidanceBlock struct {
	node *yaml.Node
}

// UnmarshalYAML implements yaml.Unmarshaler. It never returns an error.
func (g *GuidanceBlock) UnmarshalYAML(n *yaml.Node) error {
	g.node = n
	return nil
}

// IsZero lets `omitempty` drop an absent block.
func (g GuidanceBlock) IsZero() bool { return g.node == nil }

// guidanceEntry is one key of a block as written: its raw value and, when the
// value is a usable path, that path.
type guidanceEntry struct {
	key     string
	raw     *yaml.Node
	path    string // "" when the value is refused
	problem string // why the value is refused, "" when it is usable
}

// entries decodes the block. Unknown keys are reported separately by
// guidanceProblems and never returned here.
func (g GuidanceBlock) entries() []guidanceEntry {
	if g.node == nil || g.node.Kind != yaml.MappingNode {
		return nil
	}
	var out []guidanceEntry
	for i := 0; i+1 < len(g.node.Content); i += 2 {
		k, v := g.node.Content[i].Value, g.node.Content[i+1]
		if !slices.Contains(guidanceKinds, k) {
			continue
		}
		e := guidanceEntry{key: k, raw: v}
		if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
			e.problem = "must be a path under " + GuidanceDir + "/ (like guidance/x.md), not " + yamlKindName(v)
		} else if p := guidancePathProblem(v.Value); p != "" {
			e.problem = p
		} else {
			e.path = v.Value
		}
		out = append(out, e)
	}
	return out
}

func yamlKindName(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	case yaml.AliasNode:
		return "an alias"
	}
	return fmt.Sprintf("a %s value", strings.TrimPrefix(n.Tag, "!!"))
}

// ValidGuidancePath reports whether v is a usable guidance path.
func ValidGuidancePath(v string) bool { return guidancePathProblem(v) == "" }

// guidancePathProblem says why v is not a usable guidance path, "" when it is.
// The path is judged AS WRITTEN: it must already be clean, so nothing depends
// on a later normalisation to keep it inside .knomit/guidance/.
func guidancePathProblem(v string) string {
	const want = "must be a path under " + GuidanceDir + "/ (like guidance/x.md)"
	switch {
	case v == "":
		return want + ", got an empty value"
	case len(v) > maxGuidancePathBytes:
		return fmt.Sprintf("%s of at most %d bytes: the ontology holds the path, the text belongs in the file", want, maxGuidancePathBytes)
	case strings.ContainsFunc(v, unicode.IsControl) || !utf8.ValidString(v):
		return want + ": one line of text, not inline guidance — the text belongs in the file"
	case strings.Contains(v, `\`):
		return want + " with forward slashes"
	case strings.HasPrefix(v, "/"):
		return want + ", relative to " + PrivateRoot + "/, not absolute"
	case slices.Contains(strings.Split(v, "/"), ".."):
		return want + ": it may not leave " + GuidanceDir + "/"
	case path.Clean(v) != v:
		return want + ", written clean (no ./, // or trailing /)"
	case !strings.HasPrefix(v, "guidance/") || len(v) == len("guidance/"):
		return want
	}
	return ""
}

// GuidanceFile is the repository path a valid guidance path names.
func GuidanceFile(p string) string { return PrivateRoot + "/" + p }

// GuidanceRef is one guidance file in force for a topic.
type GuidanceRef struct {
	// Path is the value as written in the ontology (guidance/x.md).
	Path string
	// File is the repository path it names (.knomit/guidance/x.md).
	File string
	// Scope is the declaring node's topic path, "" for the root block.
	Scope string
}

// resolvedGuidance is one node's guidance in force: kind → ref. A kind that is
// declared but refused maps to a ref with an empty Path (it shadows the
// parent and resolves to nothing).
type resolvedGuidance map[string]GuidanceRef

// resolveGuidance lays node block over inherited. The returned map may be
// inherited itself — never write to it.
func resolveGuidance(scope string, b GuidanceBlock, inherited resolvedGuidance) resolvedGuidance {
	es := b.entries()
	if len(es) == 0 {
		return inherited
	}
	out := make(resolvedGuidance, len(inherited)+len(es))
	maps.Copy(out, inherited)
	for _, e := range es {
		r := GuidanceRef{Scope: scope}
		if e.path != "" {
			r.Path, r.File = e.path, GuidanceFile(e.path)
		}
		out[e.key] = r
	}
	return out
}

// GuidanceFor returns the guidance files of kind in force for topicPath: the
// root block's first, then the nearest declaration on the topic walk (the walk
// Attr uses), without duplicates. A declared-but-refused value resolves to
// nothing; an undeclared topic gets the root's only.
func (o *Ontology) GuidanceFor(topicPath, kind string) []GuidanceRef {
	if o == nil {
		return nil
	}
	var out []GuidanceRef
	if r, ok := resolveGuidance("", o.Guidance, nil)[kind]; ok && r.Path != "" {
		out = append(out, r)
	}
	if topicPath == "" {
		return out
	}
	if r, ok := o.cache.guidanceByTopic[o.resolveDeclaredPrefix(topicPath)][kind]; ok && r.Path != "" {
		if len(out) == 0 || out[0].Path != r.Path {
			out = append(out, r)
		}
	}
	return out
}

// DeclaredTopic is the deepest DECLARED topic path on topicPath's walk (the
// walk Attr uses, lowercased), "" when the topic itself is not declared. A
// prompt labels guidance with this, never with a fact's own path: a category
// is freeform and written by whoever wrote the fact.
func (o *Ontology) DeclaredTopic(topicPath string) string {
	if o == nil || topicPath == "" {
		return ""
	}
	return o.resolveDeclaredPrefix(topicPath)
}

// GuidanceDeclarations maps every usable guidance path the ontology names, at
// the root and on every node, to where it is declared ("root" or the topic
// path), sorted: what a reader fetches, and what its warnings name.
func (o *Ontology) GuidanceDeclarations() map[string][]string {
	if o == nil {
		return nil
	}
	out := map[string][]string{}
	add := func(scope string, b GuidanceBlock) {
		for _, e := range b.entries() {
			if e.path != "" && !slices.Contains(out[e.path], scope) {
				out[e.path] = append(out[e.path], scope)
			}
		}
	}
	add("root", o.Guidance)
	var walk func(prefix string, n *OntologyNode)
	walk = func(prefix string, n *OntologyNode) {
		if n == nil {
			return
		}
		add(prefix, n.Guidance)
		for k, c := range n.Children {
			walk(prefix+"/"+k, c)
		}
	}
	for k, n := range o.Topics {
		walk(k, n)
	}
	for _, scopes := range out {
		slices.Sort(scopes)
	}
	return out
}

// ValidationsFor returns the validations in force for a fact under topicPath,
// in the order ValidateFact runs them: the root's, then each declared node's
// on the walk.
func (o *Ontology) ValidationsFor(topicPath string) []Validation {
	if o == nil {
		return nil
	}
	out := slices.Clone(o.Validations)
	if topicPath == "" {
		return out
	}
	parts := strings.Split(topicPath, "/")
	node, ok := o.Topics[strings.ToLower(parts[0])]
	if !ok {
		return out
	}
	if node != nil {
		out = append(out, node.Validations...)
	}
	for _, seg := range parts[1:] {
		if node == nil || node.Children == nil {
			break
		}
		child, ok := node.Children[strings.ToLower(seg)]
		if !ok {
			break
		}
		node = child
		if node != nil {
			out = append(out, node.Validations...)
		}
	}
	return out
}

// guidanceDiags checks a guidance block. Every problem is a warning on the
// open path and fatal for a new ontology. where is "root" or `topic "x/y"`.
func guidanceDiags(where string, b GuidanceBlock) []Diagnostic {
	n := b.node
	if n == nil {
		return nil
	}
	newOnly := func(at *yaml.Node, msg string) Diagnostic {
		d := diagAt(at, "parse ontology: guidance in "+where+": "+msg)
		d.Severity, d.newOnly = SeverityWarning, true
		return d
	}
	if n.Kind != yaml.MappingNode {
		return []Diagnostic{newOnly(n, fmt.Sprintf(
			"must be a mapping of %q and/or %q to paths under %s/, not %s; ignored",
			GuidanceHypothesize, GuidanceReview, GuidanceDir, yamlKindName(n)))}
	}
	var diags []Diagnostic
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if !slices.Contains(guidanceKinds, k.Value) {
			diags = append(diags, newOnly(k, fmt.Sprintf(
				"unknown key %q: this knomit knows %q and %q; ignored", k.Value, GuidanceHypothesize, GuidanceReview)))
		}
	}
	for _, e := range b.entries() {
		if e.problem != "" {
			diags = append(diags, newOnly(e.raw, fmt.Sprintf("%q %s, got %s; no %s guidance here",
				e.key, e.problem, excerpt(e.raw), e.key)))
		}
	}
	return diags
}

// guidanceTreeDiags runs guidanceDiags over node and every node below it.
func guidanceTreeDiags(topicPath string, node *OntologyNode) []Diagnostic {
	if node == nil {
		return nil
	}
	diags := guidanceDiags(fmt.Sprintf("topic %q", topicPath), node.Guidance)
	for _, ck := range sortedKeys(node.Children) {
		diags = append(diags, guidanceTreeDiags(topicPath+"/"+ck, node.Children[ck])...)
	}
	return diags
}

// excerpt quotes at most 60 bytes of a value, on one line, for a diagnostic:
// a refused 200-line paragraph must not be echoed back whole.
func excerpt(n *yaml.Node) string {
	if n.Kind != yaml.ScalarNode {
		return yamlKindName(n)
	}
	s := strings.Join(strings.Fields(n.Value), " ")
	if len(s) > 60 {
		cut := 60
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return fmt.Sprintf("%q", s)
}

// guidanceSubset reports whether a's block is safe to overwrite with b's: a
// declares none, or exactly what b declares (compared as YAML, as triggers).
func guidanceSubset(a, b GuidanceBlock) bool {
	if a.node == nil {
		return true
	}
	if b.node == nil {
		return false
	}
	ay, aerr := yaml.Marshal(a.node)
	by, berr := yaml.Marshal(b.node)
	return aerr == nil && berr == nil && string(ay) == string(by)
}
