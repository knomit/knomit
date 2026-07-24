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
	bodyStr := strings.TrimLeft(stripCitations(string(body)), "\n")
	src := "---\n" + string(yb) + "---\n" + bodyStr
	return fact.ParseFact(m.KnomitPath, src)
}

func stripCitations(body string) string {
	if i := strings.Index(body, "\n# Citations\n"); i >= 0 {
		return strings.TrimRight(body[:i], "\n") + "\n"
	}
	return body
}
