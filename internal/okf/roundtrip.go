// internal/okf/roundtrip.go
package okf

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"knomit/internal/fact"
)

// ParseConcept reconstructs a fact.Fact from a generated concept document,
// using the knomit_* keys plus the body. It is the inverse of Concept and
// exists to keep the export round-trippable before an OKF→knomit importer is
// built. It reconstructs the knomit on-disk source and hands it to ParseFact
// so the result is validated exactly as a stored fact would be.
func ParseConcept(content []byte) (fact.Fact, error) {
	fm, body, ok := splitFrontmatter(content)
	if !ok {
		return fact.Fact{}, fmt.Errorf("okf: concept has no frontmatter")
	}
	// The OKF `type` field is the singularized TOPIC (derived, discardable on
	// import); the leaf fact type is restored from knomit_type, never re-derived
	// from `type`. The topic itself is authoritative via knomit_path.
	var m struct {
		KnomitType    string   `yaml:"knomit_type"`
		KnomitKind    string   `yaml:"knomit_kind"`
		KnomitPath    string   `yaml:"knomit_path"`
		KnomitConf    float64  `yaml:"knomit_confidence"`
		KnomitEvidWt  float64  `yaml:"knomit_evidence_weight"`
		KnomitOrigin  string   `yaml:"knomit_origin"`
		KnomitSources int      `yaml:"knomit_sources"`
		KnomitDomain  []string `yaml:"knomit_domain"`
		KnomitEnts    []string `yaml:"knomit_entities"`
		KnomitRefs    []string `yaml:"knomit_refs"`
	}
	if err := yaml.Unmarshal(fm, &m); err != nil {
		return fact.Fact{}, fmt.Errorf("okf: parse concept frontmatter: %w", err)
	}

	// Reconstruct the knomit on-disk frontmatter (exact keys/types ParseFact
	// reads) and re-marshal, so the round-trip goes through the canonical parser.
	var knomitFM struct {
		Kind           string   `yaml:"kind"`
		Type           string   `yaml:"type"`
		Domain         []string `yaml:"domain,omitempty"`
		Confidence     float64  `yaml:"confidence"`
		Sources        int      `yaml:"sources"`
		Entities       []string `yaml:"entities,omitempty"`
		Refs           []string `yaml:"refs,omitempty"`
		EvidenceWeight float64  `yaml:"evidence_weight,omitempty"`
		Origin         string   `yaml:"origin,omitempty"`
	}
	knomitFM.Kind = m.KnomitKind
	knomitFM.Type = m.KnomitType
	knomitFM.Domain = m.KnomitDomain
	knomitFM.Confidence = m.KnomitConf
	knomitFM.Sources = m.KnomitSources
	knomitFM.Entities = m.KnomitEnts
	knomitFM.Refs = m.KnomitRefs
	knomitFM.EvidenceWeight = m.KnomitEvidWt
	knomitFM.Origin = m.KnomitOrigin

	yb, err := yaml.Marshal(knomitFM)
	if err != nil {
		return fact.Fact{}, fmt.Errorf("okf: re-marshal knomit frontmatter: %w", err)
	}
	// The concept body begins with a leading newline then "# Title"; ParseFact's
	// extractTitle requires the first body line to be the heading, so trim it.
	bodyStr := strings.TrimLeft(stripGenerated(string(body)), "\n")
	src := "---\n" + string(yb) + "---\n" + bodyStr
	return fact.ParseFact(m.KnomitPath, src)
}

// generatedHeadings are the sections Concept appends after a fact's authored
// body, in the exact order it appends them.
var generatedHeadings = []string{"\n# Related\n", "\n# Citations\n", "\n# Cited by\n", "\n# History\n"}

// stripGenerated returns the authored body: everything before the generated
// suffix.
//
// It peels the sections off from the END rather than cutting at the first
// heading it finds anywhere. Scanning forward truncates an authored body that
// merely CONTAINS one of these headings — inside a fenced code block, or as a
// section a writer legitimately called "Citations" — and does so silently,
// which is the failure mode this whole round-trip exists to catch.
//
// Two properties make peeling from the end exact: the sections are always
// trailing, and each one's content is a bullet list or a bold line, never a
// further top-level heading. So a candidate is accepted only when everything
// from it to the previously-accepted cut contains no other "# " heading — which
// is what rejects an authored occurrence with prose or subsections under it.
//
// The one case no rule can separate: a body whose LAST section is authored,
// named exactly like a generated one, and holds nothing but a bullet list. That
// is indistinguishable from the real thing by construction, and is why the
// importer this feeds will read knomit_* rather than re-parse prose.
func stripGenerated(body string) string {
	cut := len(body)
	for i := len(generatedHeadings) - 1; i >= 0; i-- {
		h := generatedHeadings[i]
		j := strings.LastIndex(body[:cut], h)
		if j < 0 {
			continue // this section was not emitted for this fact
		}
		if strings.Contains(body[j+len(h):cut], "\n# ") {
			continue // something below it outranks a generated section: authored
		}
		cut = j
	}
	if cut == len(body) {
		return body
	}
	return strings.TrimRight(body[:cut], "\n") + "\n"
}
