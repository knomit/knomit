// internal/okf/concept.go
package okf

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"knomit/internal/fact"
)

// ErrNoType is returned by Concept when a fact has no type; the caller skips
// and counts it rather than emitting a non-conformant document.
var ErrNoType = errors.New("okf: fact has empty type")

// conceptFrontmatter is marshaled to YAML in a fixed key order. omitempty
// keeps absent optional data out of the output for stable bytes. The knomit_*
// block preserves every fact field so the deferred importer can reconstruct
// the fact from frontmatter alone (Task 13), independent of the derived tags.
type conceptFrontmatter struct {
	Type      string        `yaml:"type"`
	Title     string        `yaml:"title,omitempty"`
	Resource  string        `yaml:"resource,omitempty"`
	Tags      []string      `yaml:"tags,omitempty"`
	Timestamp string        `yaml:"timestamp,omitempty"`
	Sources   []sourceEntry `yaml:"sources,omitempty"`
	Generated *actorStamp   `yaml:"generated,omitempty"`
	Status    string        `yaml:"status,omitempty"`

	KnomitType       string   `yaml:"knomit_type,omitempty"`
	KnomitKind       string   `yaml:"knomit_kind,omitempty"`
	KnomitConfidence float64  `yaml:"knomit_confidence"`
	KnomitEvidenceWt float64  `yaml:"knomit_evidence_weight,omitempty"`
	KnomitOrigin     string   `yaml:"knomit_origin,omitempty"`
	KnomitSources    int      `yaml:"knomit_sources,omitempty"`
	KnomitDomain     []string `yaml:"knomit_domain,omitempty"`
	KnomitEntities   []string `yaml:"knomit_entities,omitempty"`
	KnomitRefs       []string `yaml:"knomit_refs,omitempty"`
	KnomitPath       string   `yaml:"knomit_path"`
}

// sourceEntry is one OKF v0.2 `sources` entry. `resource` is REQUIRED by the
// spec and must be "a concrete artifact a consumer can follow", so only refs
// that resolve to a bundle path or an absolute URL become entries — an
// unresolvable src:// anchor is NOT laundered into a fake resource. Every ref,
// resolvable or not, still round-trips losslessly via knomit_refs.
type sourceEntry struct {
	Resource string `yaml:"resource"`
	// Title is a v0.2 credibility signal. Set only when it says something the
	// resource does not — the cited fact's title for an internal edge.
	Title string `yaml:"title,omitempty"`
}

// actorStamp is the OKF v0.2 {by, at} shape used by `generated`.
type actorStamp struct {
	By string `yaml:"by"`
	At string `yaml:"at,omitempty"`
}

// generatedBy maps a knomit origin to an OKF actor (spec §9).
//
// It deliberately NEVER emits the `human:` prefix. Consumers key trust tiers
// off that prefix, and knomit's `authored` means "hand-written by a human OR an
// agent" — indistinguishable at export time. Claiming `human:` would inflate
// every consumer's trust assessment on evidence we do not have.
func generatedBy(origin string) string {
	switch origin {
	case "distilled":
		return "process:knomit-distill"
	case "discovered":
		return "process:knomit-discover"
	case "authored", "":
		return "knomit/authored"
	default:
		return "knomit/" + origin
	}
}

// statusFor maps knomit's confidence and leaf type onto the OKF v0.2 lifecycle
// field. The spec defines absent status ⇒ stable, so only the genuinely
// provisional cases are marked: low confidence, or a hypothesis (which is
// predictive by construction). Nothing is ever marked deprecated — a retracted
// knomit fact is removed from the tree, so it never reaches the exporter.
// An ABSENT confidence (0) is not a low confidence — it means the fact never
// recorded one — so it must not be downgraded to draft.
func statusFor(confidence float64, leafType string) string {
	if leafType == "hypothesis" || (confidence > 0 && confidence < 0.5) {
		return "draft"
	}
	return "" // absent ⇒ stable
}

// singularTopic maps a knomit topic directory (first path segment under kb/)
// to its OKF `type` value. Explicit for the eight known topics so the two that
// don't singularize (architecture, meta) are handled correctly.
var singularTopic = map[string]string{
	"decisions":    "decision",
	"invariants":   "invariant",
	"gotchas":      "gotcha",
	"conventions":  "convention",
	"principles":   "principle",
	"incidents":    "incident",
	"architecture": "architecture",
	"meta":         "meta",
}

// okfType derives the OKF `type` from the fact's topic (the first path segment
// under kb/), singularized via the explicit table. An unknown topic is used
// VERBATIM: English cannot be singularized by rule, and a strip-trailing-"s"
// fallback silently mangles ordinary topics — "business" became "busines" on a
// knowledge base using a non-code ontology. A plural topic name is a perfectly
// good OKF type; a misspelt one is not. Empty/absent topic falls back to the
// leaf type (never empty here). Pure and deterministic.
func okfType(factPath, leafType string) string {
	p := strings.TrimPrefix(factPath, "kb/")
	seg := p
	if i := strings.IndexByte(p, '/'); i >= 0 {
		seg = p[:i]
	}
	if s, ok := singularTopic[seg]; ok {
		return s
	}
	if seg == "" {
		return leafType
	}
	return seg
}

// RenderOpts carries the cross-document knowledge a single concept cannot have
// on its own. Both resolvers are optional; a nil resolver means "leave that
// citation kind inert" — never "emit a guess".
type RenderOpts struct {
	// ResolveFact maps a knomit fact path (kb/…) to the target document. Only
	// Build knows this, because both the filename and the title come from the
	// TARGET fact.
	ResolveFact func(knomitPath string) (target FactRef, ok bool)
	// ResolveSource maps a src://<slug>/<path>@<commit> anchor to a forge
	// permalink. Requires a slug→repo mapping; absent ⇒ inert.
	ResolveSource func(ref string) (url string, ok bool)
	// ResolveHub maps a ("domain"|"entity", key) pair to the hub page for that
	// key, when one exists. Frontmatter cannot carry markdown links — YAML
	// values are strings — so knomit_domain/knomit_entities stay machine-
	// readable data and this drives a navigable body section instead.
	ResolveHub func(kind, key string) (bundlePath string, ok bool)
	// LocalRepoID is the exporting repo's 12-hex id, so a canonical
	// kb://<own-id>/… ref resolves to a bundle document like its bare
	// equivalent. Empty ⇒ every kb:// ref reads as foreign and exports inert,
	// which is the safe direction: a bundle cannot link into a repo it does
	// not contain.
	LocalRepoID string
	// ResolveCiters returns the facts whose refs point AT this one. Only Build
	// can know this: it requires every fact's refs, inverted. Incoming edges
	// are what make the derivation graph traversable in both directions.
	ResolveCiters func(knomitPath string) []FactRef
	// Ontology supplies the authored descriptions for the knowledge scheme and
	// each of its topics/categories. Zero value ⇒ indexes carry no prose.
	Ontology OntologyDoc
	// Retired lists the facts the knowledge base has withdrawn. Build renders
	// them as ONE index page (views/retired.md) and nothing else — never as
	// concept documents, whose claims a consumer would ingest as current. Empty
	// ⇒ no page.
	Retired []Retirement
}

// Concept renders one fact as a conformant OKF concept document. fromDir is the
// document's own bundle directory, used to emit relative links (relative, not
// absolute, because GitHub resolves a leading "/" against the repo root and
// publishing to GitHub is the intended distribution path).
func Concept(fi FactInput, repo RepoIdentity, fromDir string, opts RenderOpts) ([]byte, error) {
	f := fi.Fact
	if strings.TrimSpace(string(f.Type)) == "" {
		return nil, ErrNoType
	}

	fm := conceptFrontmatter{
		Type:             okfType(f.Path(), string(f.Type)),
		Title:            f.Title,
		Resource:         fmt.Sprintf("knomit://%s/%s", repo.ID, f.Path()),
		Tags:             buildTags(f),
		Timestamp:        fi.Timestamp.UTC().Format(time.RFC3339),
		KnomitType:       string(f.Type),
		KnomitKind:       string(f.Kind),
		KnomitConfidence: f.Confidence,
		KnomitEvidenceWt: f.EvidenceWeight,
		KnomitOrigin:     string(f.Origin),
		KnomitSources:    f.Sources,
		KnomitDomain:     f.Domain,
		KnomitEntities:   f.Entities,
		KnomitRefs:       f.Refs,
		KnomitPath:       f.Path(),
	}
	// Deduplicated for the two places a ref is PRESENTED — sources[] and the
	// Citations list — since a fact naming the same ref twice would otherwise
	// emit it twice in both. KnomitRefs above stays verbatim: it is the lossless
	// round-trip channel, so a repeat there is data rather than noise.
	refs := dedupeStable(f.Refs)
	fm.Sources = buildSources(refs, fromDir, opts)
	fm.Generated = &actorStamp{
		By: generatedBy(string(f.Origin)),
		At: fi.Timestamp.UTC().Format(time.RFC3339),
	}
	fm.Status = statusFor(f.Confidence, string(f.Type))

	var yb bytes.Buffer
	enc := yaml.NewEncoder(&yb)
	enc.SetIndent(2)
	if err := enc.Encode(fm); err != nil {
		return nil, fmt.Errorf("okf: encode frontmatter: %w", err)
	}
	_ = enc.Close()

	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(yb.Bytes())
	out.WriteString("---\n\n")
	title := f.Title
	if title == "" {
		title = string(f.Type)
	}
	fmt.Fprintf(&out, "# %s\n\n", title)
	out.WriteString(strings.TrimRight(f.Body, "\n"))
	out.WriteString("\n")
	if rel := renderRelated(f, fromDir, opts); rel != "" {
		out.WriteString("\n# Related\n\n")
		out.WriteString(rel)
	}
	if len(refs) > 0 {
		out.WriteString("\n# Citations\n\n")
		for _, r := range refs {
			out.WriteString("- " + renderCitation(r, fromDir, opts) + "\n")
		}
	}
	if opts.ResolveCiters != nil {
		if citers := opts.ResolveCiters(f.Path()); len(citers) > 0 {
			out.WriteString("\n# Cited by\n\n")
			for _, c := range citers {
				out.WriteString("- [" + escapeLinkText(c.Title) + "](" + relLink(fromDir, c.Path) + ")\n")
			}
		}
	}
	if hist := renderHistory(fi.Revisions); hist != "" {
		out.WriteString("\n" + hist)
	}
	return out.Bytes(), nil
}

// renderRelated emits the navigable view of a fact's domains and entities.
//
// The same values live in knomit_domain / knomit_entities, but frontmatter
// cannot hold links — a YAML value is a string, so a markdown link there would
// render as literal text. Keeping the frontmatter as clean machine-readable
// data and putting navigation in the body serves both audiences.
//
// Only keys with a hub page are linked; the rest render as plain text rather
// than as links to documents that do not exist.
func renderRelated(f fact.Fact, fromDir string, opts RenderOpts) string {
	line := func(label, kind string, keys []string) string {
		if len(keys) == 0 {
			return ""
		}
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			if opts.ResolveHub != nil {
				if p, ok := opts.ResolveHub(kind, k); ok {
					parts = append(parts, "["+escapeLinkText(k)+"]("+relLink(fromDir, p)+")")
					continue
				}
			}
			parts = append(parts, k)
		}
		return "**" + label + ":** " + strings.Join(parts, ", ") + "\n"
	}
	var b strings.Builder
	b.WriteString(line("Domains", "domain", dedupe(f.Domain)))
	if s := line("Entities", "entity", dedupe(f.Entities)); s != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	return b.String()
}

// buildSources projects the refs that resolve to a followable artifact into
// OKF v0.2 `sources` entries. Unresolvable anchors are omitted rather than
// given a resource a consumer cannot follow; they remain visible to humans in
// the body's Citations section and lossless in knomit_refs.
func buildSources(refs []string, fromDir string, opts RenderOpts) []sourceEntry {
	var out []sourceEntry
	for _, r := range refs {
		if res, ok := resolveRef(r, fromDir, opts); ok {
			// title is set only when it adds something the resource does not —
			// for an internal fact edge, what the cited document says. Echoing
			// a URL back as its own title would be noise.
			out = append(out, sourceEntry{Resource: res.target, Title: res.title})
		}
	}
	return out
}

// resolvedRef is a citation resolved to something followable, with a
// human-readable label when one is known.
type resolvedRef struct {
	target string // link target: relative bundle path or absolute URL
	title  string // display label; empty when the target is self-describing
}

// resolveRef returns the followable resource for a ref, if there is one:
// a relative bundle path for an internal kb/ fact edge, or the URL itself for
// an http(s) ref. src:// anchors resolve only when a source resolver is
// configured — never by guessing.
func resolveRef(ref, fromDir string, opts RenderOpts) (resolvedRef, bool) {
	// Dispatch on fact.ClassifyRef so the export cannot drift from the rest of
	// the system. The prefix tests this replaced missed the canonical
	// kb://<id>/… form entirely — such a ref matched no case and silently
	// exported as an unfollowable citation.
	c := fact.ClassifyRef(ref, opts.LocalRepoID)
	switch c.Kind {
	case fact.RefLocalFact:
		if opts.ResolveFact != nil {
			if target, ok := opts.ResolveFact(c.Path); ok {
				return resolvedRef{target: relLink(fromDir, target.Path), title: target.Title}, true
			}
		}
	case fact.RefExternalURL:
		// Only http(s) is followable in a bundle; file:/// is not.
		if strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "http://") {
			return resolvedRef{target: ref}, true
		}
	case fact.RefSourceCode:
		if opts.ResolveSource != nil {
			if url, ok := opts.ResolveSource(ref); ok {
				return resolvedRef{target: url}, true
			}
		}
	}
	return resolvedRef{}, false
}

// renderCitation turns one knomit ref into the most followable markdown it can:
//
//   - kb/… — a fact in THIS bundle: a relative link to its concept document,
//     which is what makes knomit's derivation graph navigable in the export.
//   - http(s):// — an ordinary external link.
//   - src://<slug>/<path>@<commit> — a forge permalink when the bundle can
//     resolve the slug to a hosted repo, else left inert. It is deliberately
//     NOT guessed from the bundle's own remote: a KB repo is usually a
//     different repo from the code it documents, so guessing yields
//     plausible-looking links to paths that do not exist.
//
// Anything unrecognized stays an inert code span rather than becoming a
// broken link.
func renderCitation(ref, fromDir string, opts RenderOpts) string {
	res, ok := resolveRef(ref, fromDir, opts)
	if !ok {
		return "`" + ref + "`"
	}
	// Prefer the target's title: a citation labelled with a raw fact path
	// ("kb/technology/ai/models/meta/56ec386e.md") tells the reader nothing
	// about what they would be opening.
	label := firstNonEmpty(res.title, ref)
	return "[" + escapeLinkText(label) + "](" + res.target + ")"
}

// buildTags concatenates domain, entities, and kind in that order, dropping
// empties and de-duplicating while preserving first-seen order.
func buildTags(f fact.Fact) []string {
	var tags []string
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		tags = append(tags, v)
	}
	for _, d := range f.Domain {
		add(d)
	}
	for _, e := range f.Entities {
		add(e)
	}
	add(string(f.Kind))
	return tags
}
