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
	Type             string   `yaml:"type"`
	Title            string   `yaml:"title,omitempty"`
	Resource         string   `yaml:"resource,omitempty"`
	Tags             []string `yaml:"tags,omitempty"`
	Timestamp        string   `yaml:"timestamp,omitempty"`
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
// under kb/), singularized. Unknown future topics: strip one trailing 's'.
// Empty/absent topic falls back to the leaf type (never empty here). Pure and
// deterministic.
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
	if strings.HasSuffix(seg, "s") && len(seg) > 1 {
		return seg[:len(seg)-1]
	}
	return seg
}

// RenderOpts carries the cross-document knowledge a single concept cannot have
// on its own. Both resolvers are optional; a nil resolver means "leave that
// citation kind inert" — never "emit a guess".
type RenderOpts struct {
	// ResolveFact maps a knomit fact path (kb/…) to its bundle path. Only
	// Build knows this, because a fact's filename derives from the TARGET
	// fact's title.
	ResolveFact func(knomitPath string) (bundlePath string, ok bool)
	// ResolveSource maps a src://<slug>/<path>@<commit> anchor to a forge
	// permalink. Requires a slug→repo mapping; absent ⇒ inert.
	ResolveSource func(ref string) (url string, ok bool)
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
	if len(f.Refs) > 0 {
		out.WriteString("\n# Citations\n\n")
		for _, r := range f.Refs {
			out.WriteString("- " + renderCitation(r, fromDir, opts) + "\n")
		}
	}
	return out.Bytes(), nil
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
	switch {
	case strings.HasPrefix(ref, "kb/"):
		if opts.ResolveFact != nil {
			if target, ok := opts.ResolveFact(ref); ok {
				return "[" + escapeLinkText(ref) + "](" + relLink(fromDir, target) + ")"
			}
		}
	case strings.HasPrefix(ref, "https://"), strings.HasPrefix(ref, "http://"):
		return "[" + escapeLinkText(ref) + "](" + ref + ")"
	case strings.HasPrefix(ref, "src://"):
		if opts.ResolveSource != nil {
			if url, ok := opts.ResolveSource(ref); ok {
				return "[" + escapeLinkText(ref) + "](" + url + ")"
			}
		}
	}
	return "`" + ref + "`"
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
