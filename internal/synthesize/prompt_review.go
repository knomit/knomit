package synthesize

import (
	"encoding/json"
	"fmt"
)

// WorkItemContent is what gets returned to the hosting model in the MCP response.
type WorkItemContent struct {
	Prompt         string `json:"prompt"`
	ResponseSchema string `json:"response_schema"`
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
func RenderPruneWorkItem(facts []factForLLM) (*WorkItemContent, error) {
	factsJSON, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal facts for prune work item: %w", err)
	}

	prompt, err := RenderTemplate("prune", "user", PromptData{Facts: string(factsJSON)})
	if err != nil {
		return nil, fmt.Errorf("render prune work item: %w", err)
	}

	return &WorkItemContent{
		Prompt:         prompt,
		ResponseSchema: pruneResponseSchema,
	}, nil
}

// RenderDistillWorkItem renders a distill prompt for the hosting model.
func RenderDistillWorkItem(facts []factForLLM) (*WorkItemContent, error) {
	factsJSON, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal facts for distill work item: %w", err)
	}

	prompt, err := RenderTemplate("distill", "user", PromptData{Facts: string(factsJSON)})
	if err != nil {
		return nil, fmt.Errorf("render distill work item: %w", err)
	}

	return &WorkItemContent{
		Prompt:         prompt,
		ResponseSchema: distillResponseSchema,
	}, nil
}
