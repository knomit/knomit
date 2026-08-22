package synthesize

import (
	"encoding/json"
	"fmt"
)

// WorkItemContent is what gets returned to the hosting model in the MCP response.
type WorkItemContent struct {
	Prompt         string `json:"prompt"`
	ResponseSchema string `json:"response_schema"`
	// Facts is the item's payload as a structured JSON array, carried beside
	// the prompt rather than serialized into it. Empty for step types that
	// still interpolate their payload into the template.
	Facts string `json:"facts,omitempty"`
}

const pruneResponseSchema = `{
  "type": "object",
  "properties": {
    "decisions": {
      "type": "array",
      "items": {
        "properties": {
          "path": {"type": "string"},
          "action": {"type": "string", "enum": ["keep", "retract", "update"]},
          "confidence": {"type": "number"}
        },
        "required": ["path", "action"]
      }
    },
    "merges": {
      "type": "array",
      "items": {
        "properties": {
          "paths": {"type": "array", "items": {"type": "string"}},
          "merged": {
            "properties": {
              "path": {"type": "string"},
              "title": {"type": "string"},
              "body": {"type": "string"},
              "type": {"type": "string"},
              "domain": {"type": "array", "items": {"type": "string"}},
              "confidence": {"type": "number"},
              "sources": {"type": "integer"},
              "entities": {"type": "array", "items": {"type": "string"}},
              "motifs": {"type": "array", "items": {"type": "string"}, "description": "Motifs name the general regularity the claim instantiates, independent of its subject. Carry over member motifs still true of the new claim; author a new one only if the merged claim exemplifies a regularity no member named; at most 3; zero is correct."},
              "refs": {"type": "array", "items": {"type": "string"}}
            },
            "required": ["path", "title", "body"]
          }
        },
        "required": ["paths", "merged"]
      }
    }
  },
  "required": ["decisions"]
}`

const distillResponseSchema = `{
  "type": "object",
  "properties": {
    "synthesize": {
      "type": "array",
      "items": {
        "properties": {
          "path": {"type": "string"},
          "title": {"type": "string"},
          "body": {"type": "string"},
          "type": {"type": "string"},
          "domain": {"type": "array", "items": {"type": "string"}},
          "confidence": {"type": "number"},
          "entities": {"type": "array", "items": {"type": "string"}},
          "motifs": {"type": "array", "items": {"type": "string"}, "description": "Motifs name the general regularity the claim instantiates, independent of its subject. Carry over member motifs still true of the new claim; author a new one only if the synthesized claim exemplifies a regularity no member named; at most 3; zero is correct."},
          "refs": {"type": "array", "items": {"type": "string"}}
        },
        "required": ["path", "title", "body"]
      }
    },
    "retract": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["synthesize"]
}`

// RenderPruneWorkItem renders a prune prompt for the hosting model.
// ontologyRoot is substituted into the prompt's example paths so the LLM
// emits paths under the configured root instead of a hardcoded placeholder.
// Facts travel in WorkItemContent.Facts rather than interpolated into the
// prompt, for the same reasons as distill — and with more at stake. Prune
// clusters are deliberately never split across work items, because a duplicate
// pair straddling the split would simply never be found, so cluster size was
// bounded only by whatever Louvain happened to produce and had no chunkFacts
// backstop beneath it. Shipping facts structurally is what lets an oversized
// cluster be delivered across pages while the merge decision still sees all of
// it: the delivery splits, the decision does not.
func RenderPruneWorkItem(facts []factForLLM, ontologyRoot string) (*WorkItemContent, error) {
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return nil, fmt.Errorf("marshal facts for prune work item: %w", err)
	}

	prompt, err := RenderTemplate("prune", "user", PromptData{OntologyRoot: ontologyRoot})
	if err != nil {
		return nil, fmt.Errorf("render prune work item: %w", err)
	}

	return &WorkItemContent{
		Prompt:         prompt,
		ResponseSchema: pruneResponseSchema,
		Facts:          string(factsJSON),
	}, nil
}

const reflectResponseSchema = `{
  "type": "object",
  "properties": {
    "reasoning": {
      "type": "string",
      "description": "Free-form reflection on the transitions and which methodologies they reinforce or expose gaps in."
    },
    "reinforce": {
      "type": "array",
      "description": "Existing methodologies whose lesson is re-confirmed by these transitions. This is the default action — most reflections should be reinforcements.",
      "items": {
        "type": "object",
        "properties": {
          "methodology_path": {"type": "string"},
          "transition_paths": {"type": "array", "items": {"type": "string"}, "minItems": 1},
          "rationale": {"type": "string"}
        },
        "required": ["methodology_path", "transition_paths", "rationale"]
      }
    },
    "propose": {
      "type": "array",
      "description": "New methodology to add when no existing one captures the lesson. Rare. Capped at 1 by default; the server rejects proposals too similar to existing methodologies.",
      "maxItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "title": {"type": "string"},
          "body": {"type": "string"},
          "topic_path": {"type": "string", "description": "Directory under the ontology root, e.g. \"meta/reasoning\"; the server appends a UUID and writes the fact."},
          "confidence": {"type": "number", "minimum": 0, "maximum": 1},
          "domain": {"type": "array", "items": {"type": "string"}},
          "entities": {"type": "array", "items": {"type": "string"}},
          "refs": {"type": "array", "items": {"type": "string"}},
          "transition_paths": {"type": "array", "items": {"type": "string"}, "minItems": 1},
          "novelty_argument": {"type": "string", "description": "Why no existing methodology in the prompt's candidates section captures this lesson."}
        },
        "required": ["title", "body", "topic_path", "transition_paths", "novelty_argument"]
      }
    }
  },
  "required": ["reasoning", "reinforce", "propose"]
}`

// RenderReflectWorkItem renders a reflect prompt for hypothesis transition
// review. existingMethodology is the pre-formatted methodology section to
// inject; pass an empty string when none is relevant.
func RenderReflectWorkItem(transitionsJSON []byte, ontologyRoot, existingMethodology, motifVocabulary string) (*WorkItemContent, error) {
	prompt, err := RenderTemplate("reflect", "user", PromptData{
		Facts:               string(transitionsJSON),
		OntologyRoot:        ontologyRoot,
		ExistingMethodology: existingMethodology,
		MotifVocabulary:     motifVocabulary,
	})
	if err != nil {
		return nil, fmt.Errorf("render reflect work item: %w", err)
	}
	return &WorkItemContent{
		Prompt:         prompt,
		ResponseSchema: reflectResponseSchema,
	}, nil
}

// RenderDistillWorkItem renders a distill prompt for the hosting model.
// applicableMethodology is the pre-formatted methodology section to inject;
// pass an empty string when none is relevant.
// Facts travel in WorkItemContent.Facts rather than interpolated into the
// prompt. Two reasons, both learned from the payloads this change exists to fix:
// a fact array embedded in the prompt STRING cannot be sliced without regex
// surgery on the prompt (and arrives as a handful of enormous lines, so no
// line-based reader can window it), and every quote inside it is escaped twice
// on the wire. Compact, not indented: it ships as structural JSON now, and the
// delivering envelope does its own formatting.
func RenderDistillWorkItem(facts []factForLLM, ontologyRoot, applicableMethodology string) (*WorkItemContent, error) {
	shared := sharedClusterMotifs(facts)
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return nil, fmt.Errorf("marshal facts for distill work item: %w", err)
	}

	prompt, err := RenderTemplate("distill", "user", PromptData{
		OntologyRoot:          ontologyRoot,
		ApplicableMethodology: applicableMethodology,
		SharedMotifs:          shared,
	})
	if err != nil {
		return nil, fmt.Errorf("render distill work item: %w", err)
	}

	return &WorkItemContent{
		Prompt:         prompt,
		ResponseSchema: distillResponseSchema,
		Facts:          string(factsJSON),
	}, nil
}

// RenderMotifAliasWorkItem renders the vocabulary judge's prompt.
//
// Takes no arguments: the pairs ride in the work item's `facts` field like
// every other step's payload, and the prompt says so rather than interpolating
// them. That is also what keeps this the ONE prompt in the system that can
// legitimately contain corpus vocabulary — the vocabulary is in the payload,
// not in the template, so the MN1 enumeration over prompt TEMPLATES stays a
// clean check.
func RenderMotifAliasWorkItem() (*WorkItemContent, error) {
	prompt, err := RenderTemplate("motif_alias", "user", PromptData{})
	if err != nil {
		return nil, fmt.Errorf("render motif alias work item: %w", err)
	}
	return &WorkItemContent{
		Prompt:         prompt,
		ResponseSchema: motifAliasResponseSchema,
	}, nil
}

// RenderMotifDefineWorkItem renders the blind definition prompt.
//
// Takes no arguments for the same reason RenderMotifAliasWorkItem does: the
// names ride in the work item's `facts` field, so the TEMPLATE carries no
// corpus vocabulary and the MN1 enumeration over templates stays a clean check.
func RenderMotifDefineWorkItem() (*WorkItemContent, error) {
	prompt, err := RenderTemplate("motif_define", "user", PromptData{})
	if err != nil {
		return nil, fmt.Errorf("render motif define work item: %w", err)
	}
	return &WorkItemContent{
		Prompt:         prompt,
		ResponseSchema: motifDefineResponseSchema,
	}, nil
}
