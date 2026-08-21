package mcp

import (
	"fmt"
	"strings"

	"knomit/internal/fact"
)

// This file is the single source of truth for how the fact classification
// vocabulary (kind, type, origin) is presented at the MCP boundary.
//
// The contract used to be spelled out five times: the knomit_learn JSON
// schema, the knomit_update JSON schema, the knomit_query filter descriptions,
// the server instructions, and the Go request structs. Each copy drifted
// independently — `type` had no enum at all in either schema while `kind` and
// `origin` did, and the three prose enumerations of the twelve leaf types
// disagreed on wording.
//
// The split of responsibility is deliberate:
//
//   - The ENUMS are derived from internal/fact (AllEpistemicTypes,
//     AllPragmaticTypes, AllOrigins). internal/fact owns which values exist;
//     this package must never restate them.
//   - The DESCRIPTIONS live here, not in internal/fact. They are tuned for a
//     single audience — an LLM tool-caller reading a JSON schema — and their
//     wording is driven by prompt-engineering concerns, not domain semantics.
//     internal/fact stays free of prose.
//
// What keeps the two halves honest is TestFactSchema_DescriptionsAreComplete:
// adding a Type or Origin to internal/fact without documenting it here fails
// the build's test run. Without that test this is merely shared plumbing, and
// it would drift straight back to where it started.

// typeDoc is the tool-caller-facing documentation for one leaf fact type.
type typeDoc struct {
	// Gloss answers "what is this type for" in one clause. It is the only
	// part that reaches the JSON schema, where the whole twelve-type
	// enumeration has to fit in one property description.
	Gloss string
	// Aside is the parenthetical elaboration — usually an illustrative fact
	// title, occasionally a placement or provenance note. It renders only in
	// the server instructions, which have the room to teach by example.
	Aside string
}

// factTypeDocs documents every leaf Type. Wording is inherited from the
// server instructions (historically the fullest of the three enumerations)
// rather than reinvented, so agents that learned the old phrasing still
// recognise it. Completeness against internal/fact is enforced by test.
var factTypeDocs = map[fact.Type]typeDoc{
	fact.Observation: {"concrete, specific statements", `"Alice likes Japanese tea"`},
	fact.Concept:     {"definitions, mental models", `"Japanese tea culture emphasizes mindfulness"`},
	fact.Process:     {"procedures, workflows, how-to", `"How to brew matcha"`},
	fact.Principle:   {"rules, causal claims", `"Brew below boiling to avoid bitterness"`},
	fact.Pattern:     {"recurring solutions, idioms", `"When X, do Y"`},
	fact.Reference:   {"specs, measurements, enumerations", `"Sencha steeps at 70°C for 60s"`},
	fact.Synthesis:   {"higher-order facts derived from other facts", "set automatically by the synthesize pipeline"},
	fact.Insight:     {"a non-obvious grounded conclusion drawn from connecting facts you already trust", `"X and Y together imply Z"`},
	fact.Hypothesis:  {"predictions derived from patterns — carries inherent uncertainty, not grounded in direct observation", ""},
	fact.Methodology: {"reasoning process lessons learned from hypothesis outcomes", "lives in meta/reasoning/"},
	fact.Policy:      {"mandatory rule that should always be followed", `"Always rotate secrets quarterly"`},
	fact.Heuristic:   {"rule-of-thumb to bias decisions, not absolute", `"Prefer small PRs"`},
}

// allKinds enumerates the Kinds in a stable order. Unlike Type and Origin
// there is no fact.AllKinds() to derive from — Kind is a closed two-member
// set that Kind.Validate hard-codes — so this list is checked against
// Kind.Validate by test rather than derived.
var allKinds = []fact.Kind{fact.Epistemic, fact.Pragmatic}

// factKindDocs documents each classification family.
var factKindDocs = map[fact.Kind]string{
	fact.Epistemic: "descriptive knowledge — what is",
	fact.Pragmatic: "prescriptive knowledge — what to do",
}

// factOriginDocs documents each Origin. The glosses answer "which pipeline
// minted this fact", which is the distinction agents most often get wrong —
// they reach for `discovered` when they mean "I read this somewhere".
// Completeness against fact.AllOrigins is enforced by test.
var factOriginDocs = map[fact.Origin]string{
	fact.Authored:   `hand-written by you, including a fact transcribed from an external source you read (discovered does NOT mean "I learned this from a source"); the default for every type EXCEPT synthesis, where omitting origin means distilled — so set it explicitly on a synthesis fact you wrote`,
	fact.Distilled:  "synthesis-pipeline output from a regular cluster (type synthesis only)",
	fact.Discovered: "discovery-engine output from a cross-cluster bridge (type synthesis or hypothesis only)",
}

// motifFieldDescription is blueprint §2 Block A, VERBATIM. It is SHIP text:
// the wording is the validated prompt (four models, §12-E2), not a paraphrase,
// and every rule in it is load-bearing — including the closing "an empty list
// is a correct answer", whose blunter v1 phrasing suppressed valid motifs in
// two of three models.
//
// It is STATIC. No corpus vocabulary, no examples drawn from this repo, no
// reuse-before-minting rule (roadmap MN1): a served list was measured to bias
// authors toward force-fitting a new fact onto an existing name (§12-E3), and
// cold minting converges without one (§12-E2). Phrasing convergence is
// manufactured downstream, in derived state, where being wrong costs a rebuild
// instead of a fact.
//
// Guarded byte-for-byte by TestShipBlockA_Verbatim against
// testdata/motif_block_a.txt. Do not reflow, re-punctuate, or "fix" the
// en-dashes: the goldens are a straight copy of §2 and any difference from it
// is a defect.
const motifFieldDescription = `motifs (optional, 0–3): the general regularities this fact is an instance of.
Each motif is a 2–4 word kebab-case noun phrase naming a mechanism, failure
shape, or pattern that a fact about a completely different subject could also
carry — that is the test. Examples: identifier-collision,
derived-state-liability, harness-over-model, capital-influx. NOT the fact's
subject (that is entities), NOT its area (that is domain), NOT its claim
compressed (that is the title). Omit entirely when the fact is a bare
datapoint with no general shape — an empty list is a correct answer.`

// motifsProperty returns the JSON-schema fragment for the `motifs` property,
// shared by knomit_learn and knomit_update so the two cannot drift on what the
// field means.
func motifsProperty() map[string]any {
	return map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": motifFieldDescription,
	}
}

// enumValues renders a slice of string-kinded domain values as the []string
// a JSON-schema "enum" key expects.
func enumValues[T ~string](vals []T) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = string(v)
	}
	return out
}

// allTypes returns every leaf Type, epistemic first, in internal/fact's
// stable order. This is the `type` enum: a typo'd type is now rejected at the
// protocol layer rather than surviving to fact validation.
func allTypes() []fact.Type {
	return append(fact.AllEpistemicTypes(), fact.AllPragmaticTypes()...)
}

// typeGlossList renders "name (gloss), name (gloss), …" for the given types,
// marking dflt — when it is one of them — as the value that applies when the
// property is omitted. Used inside a single-line schema description, so the
// asides are dropped.
func typeGlossList(types []fact.Type, dflt fact.Type) string {
	parts := make([]string, 0, len(types))
	for _, t := range types {
		gloss := factTypeDocs[t].Gloss
		if t == dflt {
			gloss = "default; " + gloss
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", t, gloss))
	}
	return strings.Join(parts, ", ")
}

// kindProperty returns the JSON-schema fragment for the `kind` property.
//
// dflt is emitted as the schema "default" key only when non-empty. The two
// call sites differ on purpose: knomit_learn mints a fact and so declares the
// value it will assume, while knomit_update patches an existing one, where a
// "default" would falsely read as "omit this field and it resets to
// epistemic". Defaults are therefore a parameter, not baked in.
func kindProperty(dflt fact.Kind) map[string]any {
	parts := make([]string, 0, len(allKinds))
	for _, k := range allKinds {
		label := string(k)
		if k == dflt {
			label += " (default)"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", label, factKindDocs[k]))
	}
	prop := map[string]any{
		"type": "string",
		"description": "Classification family. " + strings.Join(parts, ". ") +
			". The allowed `type` values depend on the kind; changing kind also requires a compatible type.",
		"enum": enumValues(allKinds),
	}
	if dflt != "" {
		prop["default"] = string(dflt)
	}
	return prop
}

// typeProperty returns the JSON-schema fragment for the `type` property.
// See kindProperty for why dflt is a parameter rather than a constant.
func typeProperty(dflt fact.Type) map[string]any {
	prop := map[string]any{
		"type": "string",
		"description": fmt.Sprintf("Leaf type. When kind=epistemic: %s. When kind=pragmatic: %s.",
			typeGlossList(fact.AllEpistemicTypes(), dflt),
			typeGlossList(fact.AllPragmaticTypes(), dflt)),
		"enum": enumValues(allTypes()),
	}
	if dflt != "" {
		prop["default"] = string(dflt)
	}
	return prop
}

// originGlossSentence renders "authored = …; distilled = …; discovered = …."
// Shared by the knomit_learn schema and the server instructions so the two
// cannot disagree about what an origin means.
func originGlossSentence() string {
	parts := make([]string, 0, len(fact.AllOrigins()))
	for _, o := range fact.AllOrigins() {
		parts = append(parts, fmt.Sprintf("%s = %s", o, factOriginDocs[o]))
	}
	return strings.Join(parts, "; ") + "."
}

// originProperty returns the JSON-schema fragment for the `origin` property.
// It takes no default: origin is resolved server-side by ParseFact/
// SerializeFact (authored, or distilled for legacy synthesis facts), and
// advertising a schema default would invite callers to send it explicitly.
// knomit_update has no origin property at all — origin is immutable.
func originProperty() map[string]any {
	return map[string]any{
		"type": "string",
		"description": "Which pipeline minted this fact — NOT where the information came from. " +
			"Omit for any fact you write yourself. " + originGlossSentence() +
			" Any other type with these origins is rejected. " +
			"When persisting a previewed discover/distill proposal, set the origin that work-item's prompt specifies — " +
			"origin records how the candidate group was formed, not whether it was reviewed. " +
			"origin is immutable: knomit_update cannot change it; fixing a wrong origin requires knomit_retract plus a fresh knomit_learn.",
		"enum": enumValues(fact.AllOrigins()),
	}
}

// instructionTypeLines renders the indented bullet list of leaf types for the
// server instructions, where — unlike the schema — there is room for each
// type's illustrative aside.
func instructionTypeLines(types []fact.Type, indent string) string {
	lines := make([]string, 0, len(types))
	for _, t := range types {
		doc := factTypeDocs[t]
		line := fmt.Sprintf("%s- %s: %s", indent, t, doc.Gloss)
		if doc.Aside != "" {
			line += " (" + doc.Aside + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// instructionKindLines renders the indented bullet list of kinds for the
// server instructions.
func instructionKindLines(indent string) string {
	lines := make([]string, 0, len(allKinds))
	for _, k := range allKinds {
		lines = append(lines, fmt.Sprintf("%s- %s: %s", indent, k, factKindDocs[k]))
	}
	return strings.Join(lines, "\n")
}
