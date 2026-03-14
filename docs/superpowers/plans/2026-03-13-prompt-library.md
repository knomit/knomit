# Profile-Driven Prompt Library Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a profile-driven prompt library so small models (Qwen3:8b) produce meaningful synthesize results instead of passive no-ops.

**Architecture:** Two profiles (`large`/`small`) with embedded Go `text/template` prompt files. Profile auto-detected from model name, overridable per recipe step. `CompletionOptions` struct added to `LLMAdapter.Complete` to control JSON forcing. Passive response detection + optional retry with sharper prompts.

**Tech Stack:** Go, `text/template`, `//go:embed`, uber-go/mock, zerolog

**Spec:** `docs/superpowers/specs/2026-03-13-prompt-library-design.md`

---

## Chunk 1: LLMAdapter Interface Changes

Update the `LLMAdapter` interface to add `CompletionOptions` and `Model()`. Update all 6 adapters. Regenerate mocks. Fix all tests.

### Task 1: Add CompletionOptions and Model() to the interface

**Files:**
- Modify: `internal/llm/adapter.go:40-46`

- [ ] **Step 1: Add CompletionOptions struct and update interface**

In `internal/llm/adapter.go`, add `CompletionOptions` struct and update the interface:

```go
// CompletionOptions controls provider-specific behaviour for a single call.
type CompletionOptions struct {
	ForceJSON bool // when true, constrain output to valid JSON (e.g. Ollama format:"json")
}

// LLMAdapter is the common interface implemented by every provider.
type LLMAdapter interface {
	Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error)
	Model() string
}
```

- [ ] **Step 2: Verify the file compiles in isolation**

Run: `cd /Users/knomit/data/mine/knomit && go vet ./internal/llm/adapter.go`
Expected: errors about other files not implementing the interface yet (that's OK, we'll fix them next)

### Task 2: Update all adapter implementations

**Files:**
- Modify: `internal/llm/ollama.go:83` — Complete signature + ForceJSON logic + Model()
- Modify: `internal/llm/anthropic.go:30` — Complete signature + Model()
- Modify: `internal/llm/bedrock.go:38` — Complete signature + Model()
- Modify: `internal/llm/gemini.go:39` — Complete signature + Model()
- Modify: `internal/llm/claudecli.go:29` — Complete signature + Model()
- Modify: `internal/llm/geminicli.go:25` — Complete signature + Model()

- [ ] **Step 1: Update OllamaAdapter**

In `internal/llm/ollama.go`, change Complete signature at line 83:

```go
func (a *OllamaAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
```

Change the request body construction (line 90-96) to use `opts.ForceJSON`:

```go
	format := ""
	if opts.ForceJSON {
		format = "json"
	}
	reqBody := ollamaChatRequest{
		Model:    a.model,
		Messages: chatMsgs,
		Format:   format,
		Stream:   true,
		Options:  ollamaOptions{NumPredict: defaultMaxTokens},
	}
```

Add Model() method:

```go
func (a *OllamaAdapter) Model() string { return a.model }
```

- [ ] **Step 2: Update AnthropicAdapter**

In `internal/llm/anthropic.go`, change Complete signature at line 30:

```go
func (a *AnthropicAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
```

Add Model() method:

```go
func (a *AnthropicAdapter) Model() string { return a.model }
```

- [ ] **Step 3: Update BedrockAdapter**

In `internal/llm/bedrock.go`, change Complete signature at line 38:

```go
func (a *BedrockAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
```

Add Model() method:

```go
func (a *BedrockAdapter) Model() string { return a.model }
```

- [ ] **Step 4: Update GeminiAdapter**

In `internal/llm/gemini.go`, change Complete signature at line 39:

```go
func (a *GeminiAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
```

Add Model() method:

```go
func (a *GeminiAdapter) Model() string { return a.model }
```

- [ ] **Step 5: Update ClaudeCLIAdapter**

In `internal/llm/claudecli.go`, change Complete signature at line 29:

```go
func (a *ClaudeCLIAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
```

Add Model() method:

```go
func (a *ClaudeCLIAdapter) Model() string { return a.model }
```

- [ ] **Step 6: Update GeminiCLIAdapter**

In `internal/llm/geminicli.go`, change Complete signature at line 25:

```go
func (a *GeminiCLIAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
```

Add Model() method:

```go
func (a *GeminiCLIAdapter) Model() string { return a.model }
```

### Task 3: Update all callers in synthesize package

**Files:**
- Modify: `internal/synthesize/prune.go:53-58` — add `llm.CompletionOptions{}` arg
- Modify: `internal/synthesize/distill_llm.go:96-101` — add `llm.CompletionOptions{}` arg

- [ ] **Step 1: Update prune.go Complete call**

In `internal/synthesize/prune.go`, change lines 53-58:

```go
		response, err := adapter.Complete(
			ctx,
			"You are a knowledge base maintenance assistant. Respond only with valid JSON.",
			[]llm.Message{{Role: "user", Content: prompt}},
			llm.CompletionOptions{},
			nil,
		)
```

- [ ] **Step 2: Update distill_llm.go Complete call**

In `internal/synthesize/distill_llm.go`, change lines 96-101:

```go
		response, err := adapter.Complete(
			ctx,
			"You are a knowledge base synthesis assistant. Respond only with valid JSON.",
			[]llm.Message{{Role: "user", Content: prompt}},
			llm.CompletionOptions{},
			nil,
		)
```

### Task 4: Regenerate mocks and fix tests

**Files:**
- Regenerate: `internal/llm/mock_adapter_test.go`
- Regenerate: `internal/synthesize/mock_llm_test.go`
- Modify: `internal/llm/anthropic_test.go` — update Complete call
- Modify: `internal/llm/ollama_test.go` — update Complete calls
- Modify: `internal/llm/llm_extra_test.go` — no Complete calls (factory tests only, likely no changes)
- Modify: `internal/synthesize/prune_test.go:54` — update mock expectation

- [ ] **Step 1: Regenerate LLM mock**

Run:
```bash
cd /Users/knomit/data/mine/knomit
mockgen -destination=internal/llm/mock_adapter_test.go -package=llm knomit/internal/llm LLMAdapter
```

- [ ] **Step 2: Regenerate synthesize LLM mock**

Run:
```bash
mockgen -destination=internal/synthesize/mock_llm_test.go -package=synthesize knomit/internal/llm LLMAdapter
```

- [ ] **Step 3: Update anthropic_test.go**

In `internal/llm/anthropic_test.go`, find the `Complete` call and add `CompletionOptions{}` argument. The call pattern is:

```go
result, err := adapter.Complete(ctx, "system prompt", msgs, CompletionOptions{}, func(chunk string) {
```

- [ ] **Step 4: Update ollama_test.go**

In `internal/llm/ollama_test.go`, update all `Complete` calls to include `CompletionOptions{}` as the 4th argument. There are ~6 test functions that call Complete. Each needs the extra arg:

```go
result, err := adapter.Complete(ctx, system, msgs, CompletionOptions{}, onChunk)
```

Also verify the test that checks the request body: when `CompletionOptions{}` is passed (ForceJSON=false), the `format` field in the request JSON should be `""` (empty string). Update the assertion accordingly.

**Important:** Any `DoAndReturn` closures in test files must be updated from the old 4-parameter signature to the new 5-parameter signature. For example:

```go
// OLD:
func(ctx context.Context, system string, msgs []llm.Message, onChunk func(string)) (string, error) {
// NEW:
func(ctx context.Context, system string, msgs []llm.Message, opts llm.CompletionOptions, onChunk func(string)) (string, error) {
```

Check `prune_test.go` and `pipeline_test.go` for all `DoAndReturn` closures.

- [ ] **Step 5: Update prune_test.go mock expectation**

In `internal/synthesize/prune_test.go` line 54, the mock expectation currently matches 4 args. After the interface change it needs 5 args:

```go
adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mockResponse, nil)
```

Also add a `Model()` expectation if any test calls it (check first — pipeline_test.go may need it).

- [ ] **Step 6: Run all tests**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/llm/... ./internal/synthesize/...`
Expected: all PASS

- [ ] **Step 7: Commit**

```bash
git add internal/llm/adapter.go internal/llm/ollama.go internal/llm/anthropic.go internal/llm/bedrock.go internal/llm/gemini.go internal/llm/claudecli.go internal/llm/geminicli.go internal/llm/mock_adapter_test.go internal/llm/anthropic_test.go internal/llm/ollama_test.go internal/llm/llm_extra_test.go internal/synthesize/mock_llm_test.go internal/synthesize/prune.go internal/synthesize/distill_llm.go internal/synthesize/prune_test.go
git commit -m "feat(llm): add CompletionOptions and Model() to LLMAdapter interface

Add CompletionOptions{ForceJSON} parameter to Complete() and Model() string
method. Ollama adapter reads ForceJSON instead of unconditionally forcing
JSON mode. All adapters, mocks, callers, and tests updated."
```

---

## Chunk 2: Profile System + Recipe Fields

### Task 5: Write Profile struct and ResolveProfile (TDD)

**Files:**
- Create: `internal/synthesize/profile.go`
- Create: `internal/synthesize/profile_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/synthesize/profile_test.go`:

```go
package synthesize

import "testing"

func TestResolveProfile(t *testing.T) {
	tests := []struct {
		model          string
		wantName       string
		wantJSON       bool
		wantRetry      bool
		wantChunkBytes int
	}{
		// Small models — size markers trigger small profile
		{"qwen3:8b", "small", false, true, 50_000},
		{"qwen2.5:7b", "small", false, true, 50_000},
		{"llama3:3b", "small", false, true, 50_000},
		{"mistral:7b-instruct", "small", false, true, 50_000},

		// Large models — no size marker or large size
		{"claude-sonnet-4-20250514", "large", true, false, 100_000},
		{"gpt-4o", "large", true, false, 100_000},
		{"qwen3:32b", "large", true, false, 100_000},
		{"qwen3:72b", "large", true, false, 100_000},

		// Edge cases
		{"", "large", true, false, 100_000},
		{"qwen3", "large", true, false, 100_000},       // no size marker
		{"custom-model", "large", true, false, 100_000}, // unknown model
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			p := ResolveProfile(tc.model)
			if p.Name != tc.wantName {
				t.Errorf("Name: got %q, want %q", p.Name, tc.wantName)
			}
			if p.ForceJSON != tc.wantJSON {
				t.Errorf("ForceJSON: got %v, want %v", p.ForceJSON, tc.wantJSON)
			}
			if p.RetryOnPassive != tc.wantRetry {
				t.Errorf("RetryOnPassive: got %v, want %v", p.RetryOnPassive, tc.wantRetry)
			}
			if p.MaxChunkBytes != tc.wantChunkBytes {
				t.Errorf("MaxChunkBytes: got %d, want %d", p.MaxChunkBytes, tc.wantChunkBytes)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/ -run TestResolveProfile -v`
Expected: FAIL — `ResolveProfile` not defined

- [ ] **Step 3: Write implementation**

Create `internal/synthesize/profile.go`:

```go
package synthesize

import (
	"regexp"
	"strconv"
)

// Profile captures capability-dependent settings for LLM interactions.
type Profile struct {
	Name           string // "large" or "small"
	ForceJSON      bool   // constrain output to JSON (kills chain-of-thought in small models)
	RetryOnPassive bool   // retry with sharper prompt if response is passive
	MaxChunkBytes  int    // max facts payload size per LLM call
}

// smallPattern matches model size markers like ":8b", ":7b", ":3b" where the
// number is ≤ 14. Models above 14b are treated as large.
var smallPattern = regexp.MustCompile(`:(\d+)b`)

// smallProfileThreshold is the maximum parameter count (in billions) for a model
// to be classified as "small".
const smallProfileThreshold = 14

// ResolveProfile returns the appropriate profile for a given model name.
// Models with size markers ≤ 14b are "small"; everything else is "large".
func ResolveProfile(model string) Profile {
	if isSmallModel(model) {
		return Profile{
			Name:           "small",
			ForceJSON:      false,
			RetryOnPassive: true,
			MaxChunkBytes:  50_000,
		}
	}
	return Profile{
		Name:           "large",
		ForceJSON:      true,
		RetryOnPassive: false,
		MaxChunkBytes:  100_000,
	}
}

func isSmallModel(model string) bool {
	m := smallPattern.FindStringSubmatch(model)
	if m == nil {
		return false
	}
	size, err := strconv.Atoi(m[1])
	if err != nil {
		return false
	}
	return size <= smallProfileThreshold
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/ -run TestResolveProfile -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/synthesize/profile.go internal/synthesize/profile_test.go
git commit -m "feat(synthesize): add Profile struct and ResolveProfile

Auto-detect small vs large profile from model name size markers.
Small models (≤14b) get ForceJSON=false, RetryOnPassive=true."
```

### Task 6: Add Profile and RetryOnPassive fields to RecipeStep

**Files:**
- Modify: `internal/synthesize/recipe.go:11-17`
- Modify: `internal/synthesize/recipe_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/synthesize/recipe_test.go`:

```go
func TestParseRecipeWithProfile(t *testing.T) {
	yml := `
name: profiled
steps:
  - mode: prune
    profile: small
    retry_on_passive: false
  - mode: distill
`
	r, err := ParseRecipe(yml)
	if err != nil {
		t.Fatalf("ParseRecipe: %v", err)
	}
	if r.Steps[0].Profile != "small" {
		t.Errorf("Steps[0].Profile: got %q, want %q", r.Steps[0].Profile, "small")
	}
	if r.Steps[0].RetryOnPassive == nil || *r.Steps[0].RetryOnPassive != false {
		t.Errorf("Steps[0].RetryOnPassive: want pointer to false")
	}
	if r.Steps[1].Profile != "" {
		t.Errorf("Steps[1].Profile: got %q, want empty", r.Steps[1].Profile)
	}
	if r.Steps[1].RetryOnPassive != nil {
		t.Errorf("Steps[1].RetryOnPassive: want nil")
	}
}
```

Note: `RetryOnPassive` uses `*bool` so we can distinguish "not set" (nil → use profile default) from "explicitly false".

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/ -run TestParseRecipeWithProfile -v`
Expected: FAIL — `Profile` field does not exist on RecipeStep

- [ ] **Step 3: Update RecipeStep**

In `internal/synthesize/recipe.go`, update RecipeStep:

```go
type RecipeStep struct {
	Mode           string  `yaml:"mode"`
	Model          string  `yaml:"model"`
	Prompt         string  `yaml:"prompt"`
	MaxDepth       int     `yaml:"max_depth"`
	Resolution     float64 `yaml:"resolution"`
	Profile        string  `yaml:"profile"`          // "large", "small", or "" (auto-detect)
	RetryOnPassive *bool   `yaml:"retry_on_passive"` // nil = use profile default
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/ -run TestParseRecipe -v`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/synthesize/recipe.go internal/synthesize/recipe_test.go
git commit -m "feat(synthesize): add profile and retry_on_passive to RecipeStep"
```

---

## Chunk 3: Prompt Templates

Extract current hardcoded prompts into embedded template files. Create small-model variants.

### Task 7: Create large profile prompt templates

**Files:**
- Create: `internal/synthesize/prompts/large/prune_system.txt`
- Create: `internal/synthesize/prompts/large/prune_user.txt`
- Create: `internal/synthesize/prompts/large/prune_retry.txt`
- Create: `internal/synthesize/prompts/large/distill_system.txt`
- Create: `internal/synthesize/prompts/large/distill_user.txt`
- Create: `internal/synthesize/prompts/large/distill_retry.txt`

- [ ] **Step 1: Create large/prune_system.txt**

Extract from current `prune.go:55`:

```
You are a knowledge base maintenance assistant. Respond only with valid JSON.
```

- [ ] **Step 2: Create large/prune_user.txt**

Extract from current `buildPrunePrompt` (prune_llm.go:55-102). Use `text/template` syntax:

```
You are reviewing facts in a knowledge base for staleness, redundancy, and duplication.
{{- if .RecipePrompt}}

Context: {{.RecipePrompt}}
{{- end}}
{{- if .StepPrompt}}

Instructions: {{.StepPrompt}}
{{- end}}

Facts to review:
{{.Facts}}

For each fact, decide:
- keep: fact is current and valuable
- forget: fact is obsolete, superseded, or no longer true
- update: fact needs confidence adjusted (provide new value)

Also identify facts that say the same thing and should be merged into a single unified fact.

Respond as JSON (no markdown wrapping):
{
  "decisions": [
    { "path": "...", "action": "keep|forget|update", "confidence": 0.X }
  ],
  "merges": [
    {
      "paths": ["file1.md", "file2.md"],
      "merged": {
        "path": "know/...",
        "title": "...",
        "body": "...",
        "domain": [],
        "confidence": 0.X,
        "sources": 2,
        "entities": [],
        "refs": ["file1.md", "file2.md"]
      }
    }
  ]
}
```

- [ ] **Step 3: Create large/prune_retry.txt**

```
Your previous response kept every fact and made no changes. This is not useful.

Look more carefully for:
- Redundant facts that say the same thing differently
- Outdated facts that are no longer true
- Facts that should be merged into one

Review these facts again and make real decisions:
{{.Facts}}

Respond as JSON (no markdown wrapping):
{
  "decisions": [
    { "path": "...", "action": "keep|forget|update", "confidence": 0.X }
  ],
  "merges": [...]
}
```

- [ ] **Step 4: Create large/distill_system.txt**

Extract from current `distill_llm.go:98`:

```
You are a knowledge base synthesis assistant. Respond only with valid JSON.
```

- [ ] **Step 5: Create large/distill_user.txt**

Extract from current `buildDistillPrompt` (distill_llm.go:33-71):

```
You are synthesizing facts in a knowledge base to find patterns and higher-order insights.
{{- if .RecipePrompt}}

Context: {{.RecipePrompt}}
{{- end}}
{{- if .StepPrompt}}

Instructions: {{.StepPrompt}}
{{- end}}

Facts in scope:
{{.Facts}}

Identify patterns across these facts. Produce:
1. New higher-order facts that capture patterns
2. Which original facts are fully subsumed and can be forgotten

Respond as JSON (no markdown wrapping):
{
  "synthesize": [
    {
      "path": "know/...",
      "title": "...",
      "body": "...",
      "domain": [],
      "confidence": 0.X,
      "entities": [],
      "refs": ["source-file1.md", "source-file2.md"]
    }
  ],
  "forget": ["file1.md", "file2.md"]
}
```

- [ ] **Step 6: Create large/distill_retry.txt**

```
Your previous response produced no new insights and forgot nothing. This is not useful.

Look more carefully for:
- Common themes across multiple facts
- Facts that can be generalized into a higher-order pattern
- Redundant facts that a synthesized fact would subsume

Review these facts again:
{{.Facts}}

Respond as JSON (no markdown wrapping):
{
  "synthesize": [...],
  "forget": [...]
}
```

- [ ] **Step 7: Commit**

```bash
git add internal/synthesize/prompts/large/
git commit -m "feat(synthesize): add large profile prompt templates

Extract existing hardcoded prompts into embedded template files.
These are functionally equivalent to the current inline prompts."
```

### Task 8: Create small profile prompt templates

**Files:**
- Create: `internal/synthesize/prompts/small/prune_system.txt`
- Create: `internal/synthesize/prompts/small/prune_user.txt`
- Create: `internal/synthesize/prompts/small/prune_retry.txt`
- Create: `internal/synthesize/prompts/small/distill_system.txt`
- Create: `internal/synthesize/prompts/small/distill_user.txt`
- Create: `internal/synthesize/prompts/small/distill_retry.txt`

- [ ] **Step 1: Create small/prune_system.txt**

No `ForceJSON`, so system prompt allows thinking:

```
You are a knowledge base assistant that removes outdated and duplicate facts.

Think step by step, then output JSON. You may write your reasoning first inside <think> tags, then output the JSON result.
```

- [ ] **Step 2: Create small/prune_user.txt**

Includes few-shot example and explicit criteria:

```
Review these facts. For each one, decide: keep, forget, or update.
{{- if .RecipePrompt}}

Context: {{.RecipePrompt}}
{{- end}}
{{- if .StepPrompt}}

Instructions: {{.StepPrompt}}
{{- end}}

RULES:
1. If two facts say the same thing, mark one as "forget" and keep the better one, OR merge them.
2. If a fact is clearly outdated or wrong, mark it "forget".
3. If a fact's confidence should change, mark it "update" with the new confidence value.
4. Only mark "keep" if the fact is accurate, unique, and valuable.

Facts:
{{.Facts}}

EXAMPLE OUTPUT:
{
  "decisions": [
    { "path": "know/topic/fact1.md", "action": "keep", "confidence": 0.9 },
    { "path": "know/topic/fact2.md", "action": "forget" },
    { "path": "know/topic/fact3.md", "action": "update", "confidence": 0.5 }
  ],
  "merges": [
    {
      "paths": ["know/topic/factA.md", "know/topic/factB.md"],
      "merged": {
        "path": "know/topic/factAB.md",
        "title": "Combined fact title",
        "body": "Combined fact body with information from both sources.",
        "domain": ["topic"],
        "confidence": 0.85,
        "sources": 2,
        "entities": ["entity1"],
        "refs": ["know/topic/factA.md", "know/topic/factB.md"]
      }
    }
  ]
}

Now output your JSON response (same schema as the example):
```

- [ ] **Step 3: Create small/prune_retry.txt**

```
You kept every fact and made zero changes. That means you did not actually review them.

Go back and look for ANY of these:
- Two facts that overlap or repeat the same information
- A fact that is outdated
- A fact with wrong confidence

You MUST mark at least one fact as "forget" or propose at least one merge. If every fact is truly unique and correct, explain why in a "keep" decision, but this is rare.

Facts:
{{.Facts}}

Output JSON:
{
  "decisions": [
    { "path": "...", "action": "keep|forget|update", "confidence": 0.X }
  ],
  "merges": [...]
}
```

- [ ] **Step 4: Create small/distill_system.txt**

```
You are a knowledge base assistant that finds patterns across facts and creates summaries.

Think step by step, then output JSON. You may write your reasoning first inside <think> tags, then output the JSON result.
```

- [ ] **Step 5: Create small/distill_user.txt**

```
Find patterns across these facts. Create new summary facts and list which originals are now redundant.
{{- if .RecipePrompt}}

Context: {{.RecipePrompt}}
{{- end}}
{{- if .StepPrompt}}

Instructions: {{.StepPrompt}}
{{- end}}

RULES:
1. Look for facts that share a common theme or can be generalized.
2. Create a new fact that captures the pattern.
3. List original facts that the new fact fully replaces in "forget".
4. The new fact's "refs" should list the source facts it was derived from.

Facts:
{{.Facts}}

EXAMPLE OUTPUT:
{
  "synthesize": [
    {
      "path": "know/patterns/combined-insight.md",
      "title": "Pattern: common theme across facts",
      "body": "Several facts indicate that X leads to Y. This pattern is consistent across domains A and B.",
      "domain": ["patterns"],
      "confidence": 0.8,
      "entities": ["X", "Y"],
      "refs": ["know/topic/fact1.md", "know/topic/fact2.md"]
    }
  ],
  "forget": ["know/topic/fact1.md", "know/topic/fact2.md"]
}

Now output your JSON response (same schema as the example):
```

- [ ] **Step 6: Create small/distill_retry.txt**

```
You produced no new insights and forgot nothing. That means you did not find any patterns.

Go back and look for ANY of these:
- Facts from the same domain that share a theme
- Facts that could be combined into a single higher-level insight
- Redundant facts where one generalizes the others

You MUST create at least one synthesized fact. Group related facts together.

Facts:
{{.Facts}}

Output JSON:
{
  "synthesize": [...],
  "forget": [...]
}
```

- [ ] **Step 7: Commit**

```bash
git add internal/synthesize/prompts/small/
git commit -m "feat(synthesize): add small profile prompt templates

Optimized for small models: few-shot examples, explicit criteria,
<think> preamble for chain-of-thought, direct language."
```

### Task 9: Build the template loader (TDD)

**Files:**
- Create: `internal/synthesize/prompts.go`
- Create: `internal/synthesize/prompts_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/synthesize/prompts_test.go`:

```go
package synthesize

import "testing"

func TestRenderTemplate(t *testing.T) {
	data := PromptData{
		Facts:        `[{"file":"test.md","title":"Test"}]`,
		RecipePrompt: "summarize everything",
		StepPrompt:   "be thorough",
	}

	// Test large prune user template renders with all fields
	out, err := RenderTemplate("large", "prune", "user", data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if out == "" {
		t.Fatal("empty output")
	}
	if !contains(out, data.Facts) {
		t.Error("output missing facts")
	}
	if !contains(out, data.RecipePrompt) {
		t.Error("output missing recipe prompt")
	}
}

func TestRenderTemplate_SmallHasExample(t *testing.T) {
	data := PromptData{Facts: `[]`}
	out, err := RenderTemplate("small", "prune", "user", data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if !contains(out, "EXAMPLE OUTPUT") {
		t.Error("small prune_user should contain EXAMPLE OUTPUT")
	}
}

func TestRenderTemplate_SystemPrompt(t *testing.T) {
	data := PromptData{}
	out, err := RenderTemplate("large", "prune", "system", data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if out == "" {
		t.Fatal("empty system prompt")
	}
}

func TestRenderTemplate_InvalidProfile(t *testing.T) {
	_, err := RenderTemplate("nonexistent", "prune", "user", PromptData{})
	if err == nil {
		t.Error("expected error for invalid profile")
	}
}

func TestRenderTemplate_AllTemplatesExist(t *testing.T) {
	profiles := []string{"large", "small"}
	ops := []string{"prune", "distill"}
	types := []string{"system", "user", "retry"}

	for _, p := range profiles {
		for _, op := range ops {
			for _, tp := range types {
				_, err := RenderTemplate(p, op, tp, PromptData{Facts: "[]"})
				if err != nil {
					t.Errorf("RenderTemplate(%s, %s, %s) failed: %v", p, op, tp, err)
				}
			}
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

Note: Use `strings.Contains` instead of the manual `contains` helper — the above is just illustrative. The actual test should import `"strings"` and use `strings.Contains`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/ -run TestRenderTemplate -v`
Expected: FAIL — `RenderTemplate` not defined

- [ ] **Step 3: Write implementation**

Create `internal/synthesize/prompts.go`:

```go
package synthesize

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed prompts/large/*.txt prompts/small/*.txt
var promptFS embed.FS

// PromptData is the data passed to prompt templates.
type PromptData struct {
	Facts        string
	RecipePrompt string
	StepPrompt   string
}

// RenderTemplate loads and renders a prompt template.
// profile: "large" or "small"
// operation: "prune" or "distill"
// promptType: "system", "user", or "retry"
func RenderTemplate(profile, operation, promptType string, data PromptData) (string, error) {
	path := fmt.Sprintf("prompts/%s/%s_%s.txt", profile, operation, promptType)
	raw, err := promptFS.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("load template %s: %w", path, err)
	}

	tmpl, err := template.New(path).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", path, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render template %s: %w", path, err)
	}
	return buf.String(), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/ -run TestRenderTemplate -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/synthesize/prompts.go internal/synthesize/prompts_test.go
git commit -m "feat(synthesize): add template loader with //go:embed

RenderTemplate loads profile-specific prompt templates and renders
them with PromptData (Facts, RecipePrompt, StepPrompt)."
```

---

## Chunk 4: Validation + extractJSON Enhancement

### Task 10: Extend extractJSON to strip `<think>` blocks (TDD)

**Files:**
- Modify: `internal/synthesize/prune_llm.go:105-121`
- Modify: `internal/synthesize/synthesize_extra_test.go` (or create new test)

- [ ] **Step 1: Write the failing test**

Add tests for `<think>` stripping. Find or create an appropriate test file:

```go
func TestExtractJSON_ThinkBlocks(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "think then JSON",
			input: "<think>\nLet me analyze these facts...\n</think>\n{\"decisions\": []}",
			want:  `{"decisions": []}`,
		},
		{
			name:  "think then fenced JSON",
			input: "<think>\nreasoning here\n</think>\n```json\n{\"decisions\": []}\n```",
			want:  `{"decisions": []}`,
		},
		{
			name:  "no think block",
			input: `{"decisions": []}`,
			want:  `{"decisions": []}`,
		},
		{
			name:  "fenced without think",
			input: "```json\n{\"foo\": 1}\n```",
			want:  `{"foo": 1}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSON(tc.input)
			if got != tc.want {
				t.Errorf("extractJSON:\ngot:  %q\nwant: %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/ -run TestExtractJSON_ThinkBlocks -v`
Expected: FAIL on "think then JSON" case

- [ ] **Step 3: Update extractJSON**

In `internal/synthesize/prune_llm.go`, update `extractJSON`:

```go
func extractJSON(text string) string {
	text = strings.TrimSpace(text)
	// Strip <think>...</think> blocks (used by small models for chain-of-thought)
	if idx := strings.Index(text, "</think>"); idx >= 0 {
		text = strings.TrimSpace(text[idx+len("</think>"):])
	}
	// Strip ```json ... ``` or ``` ... ```
	if strings.HasPrefix(text, "```") {
		end := strings.LastIndex(text, "```")
		if end > 3 {
			inner := text[3:end]
			if idx := strings.IndexByte(inner, '\n'); idx >= 0 {
				inner = inner[idx+1:]
			}
			return strings.TrimSpace(inner)
		}
	}
	return text
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/ -run TestExtractJSON -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/synthesize/prune_llm.go internal/synthesize/synthesize_extra_test.go
git commit -m "feat(synthesize): extend extractJSON to strip <think> blocks

Small models use chain-of-thought reasoning inside <think> tags
before emitting JSON. extractJSON now strips these before parsing."
```

### Task 11: Add passive response detection (TDD)

**Files:**
- Create: `internal/synthesize/validation.go`
- Create: `internal/synthesize/validation_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/synthesize/validation_test.go`:

```go
package synthesize

import "testing"

func TestIsPrunePassive(t *testing.T) {
	tests := []struct {
		name   string
		result PruneResult
		want   bool
	}{
		{
			name: "all keep, no merges — passive",
			result: PruneResult{
				Decisions: []PruneDecision{
					{Path: "a.md", Action: "keep"},
					{Path: "b.md", Action: "keep"},
				},
			},
			want: true,
		},
		{
			name: "has forget — active",
			result: PruneResult{
				Decisions: []PruneDecision{
					{Path: "a.md", Action: "keep"},
					{Path: "b.md", Action: "forget"},
				},
			},
			want: false,
		},
		{
			name: "has update — active",
			result: PruneResult{
				Decisions: []PruneDecision{
					{Path: "a.md", Action: "update", Confidence: 0.5},
				},
			},
			want: false,
		},
		{
			name: "has merge — active",
			result: PruneResult{
				Decisions: []PruneDecision{
					{Path: "a.md", Action: "keep"},
				},
				Merges: []MergeEntry{{Paths: []string{"a.md", "b.md"}}},
			},
			want: false,
		},
		{
			name:   "empty decisions — passive",
			result: PruneResult{},
			want:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPrunePassive(tc.result); got != tc.want {
				t.Errorf("isPrunePassive = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsDistillPassive(t *testing.T) {
	tests := []struct {
		name       string
		result     DistillResult
		inputPaths []string
		want       bool
	}{
		{
			name:       "empty synthesize — passive",
			result:     DistillResult{},
			inputPaths: []string{"a.md"},
			want:       true,
		},
		{
			name: "has new facts and forgets — active",
			result: DistillResult{
				Synthesize: []distillFact{{Path: "new.md", Title: "New"}},
				Forget:     []string{"a.md"},
			},
			inputPaths: []string{"a.md"},
			want:       false,
		},
		{
			name: "synthesized paths match inputs, no forget — passive",
			result: DistillResult{
				Synthesize: []distillFact{
					{Path: "a.md", Title: "Same"},
					{Path: "b.md", Title: "Same"},
				},
			},
			inputPaths: []string{"a.md", "b.md"},
			want:       true,
		},
		{
			name: "synthesized includes new path — active",
			result: DistillResult{
				Synthesize: []distillFact{
					{Path: "new.md", Title: "New insight"},
				},
			},
			inputPaths: []string{"a.md", "b.md"},
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDistillPassive(tc.result, tc.inputPaths); got != tc.want {
				t.Errorf("isDistillPassive = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/ -run "TestIsPrunePassive|TestIsDistillPassive" -v`
Expected: FAIL — functions not defined

- [ ] **Step 3: Write implementation**

Create `internal/synthesize/validation.go`:

```go
package synthesize

// isPrunePassive returns true if the prune result made no meaningful changes:
// all decisions are "keep" (or empty) and no merges proposed.
func isPrunePassive(r PruneResult) bool {
	if len(r.Merges) > 0 {
		return false
	}
	for _, d := range r.Decisions {
		if d.Action != "keep" {
			return false
		}
	}
	return true
}

// isDistillPassive returns true if the distill result produced no new insights:
// empty synthesize array, or all synthesized paths match input paths with no forgets.
func isDistillPassive(r DistillResult, inputPaths []string) bool {
	if len(r.Synthesize) == 0 {
		return true
	}
	if len(r.Forget) > 0 {
		return false
	}
	// Check if all synthesized paths are just echoing input paths
	inputSet := make(map[string]bool, len(inputPaths))
	for _, p := range inputPaths {
		inputSet[p] = true
	}
	for _, s := range r.Synthesize {
		if !inputSet[s.Path] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/ -run "TestIsPrunePassive|TestIsDistillPassive" -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/synthesize/validation.go internal/synthesize/validation_test.go
git commit -m "feat(synthesize): add passive response detection

isPrunePassive and isDistillPassive detect no-op LLM responses
so the retry flow can trigger a sharper follow-up prompt."
```

---

## Chunk 5: Wire Everything Together

### Task 12: Update prune step to use profiles, templates, and retry

**Files:**
- Modify: `internal/synthesize/prune.go`
- Modify: `internal/synthesize/prune_llm.go`
- Modify: `internal/synthesize/pipeline.go`

- [ ] **Step 1: Add resolveStepProfile helper to pipeline.go**

In `internal/synthesize/pipeline.go`, add a helper that resolves the profile for a step:

```go
// resolveStepProfile returns the profile for a step, using the step's explicit
// override or auto-detecting from the adapter's model name.
var namedProfiles = map[string]Profile{
	"small": {Name: "small", ForceJSON: false, RetryOnPassive: true, MaxChunkBytes: 50_000},
	"large": {Name: "large", ForceJSON: true, RetryOnPassive: false, MaxChunkBytes: 100_000},
}

func resolveStepProfile(step RecipeStep, adapter llm.LLMAdapter) Profile {
	var p Profile
	if named, ok := namedProfiles[step.Profile]; ok {
		p = named
	} else {
		p = ResolveProfile(adapter.Model())
	}
	if step.RetryOnPassive != nil {
		p.RetryOnPassive = *step.RetryOnPassive
	}
	return p
}
```

- [ ] **Step 2: Thread profile into executePruneStep and executeDistillStep**

In `pipeline.go`, update the `Run` loop to resolve and pass the profile:

```go
	for i, step := range r.Steps {
		profile := resolveStepProfile(step, adapter)
		log.Info().Str("mode", step.Mode).Str("profile", profile.Name).Int("step", i+1).Int("total", len(r.Steps)).Msg("synthesis: step starting")
		onProgress(ProgressEvent{Phase: "step-start", Message: step.Mode})
		var err error
		switch step.Mode {
		case "prune":
			err = executePruneStep(ctx, gs, idx, embedder, adapter, step, r, profile, onProgress)
		case "distill":
			err = executeDistillStep(ctx, gs, idx, embedder, adapter, step, r, profile, onProgress)
		...
```

Update both function signatures to accept `profile Profile` as a parameter.

- [ ] **Step 3: Update prune.go to use templates, profile, and retry**

In `internal/synthesize/prune.go`, update `executePruneStep` signature to include `profile Profile`. Replace the hardcoded `maxChunkBytes` with `profile.MaxChunkBytes`. Replace the hardcoded prompt construction and Complete call with template rendering:

```go
func executePruneStep(ctx context.Context, gs GitStore, idx SearchIndex, embedder Embedder, adapter llm.LLMAdapter, step RecipeStep, recipe Recipe, profile Profile, onProgress func(ProgressEvent)) error {
```

For each chunk, replace the prompt building + LLM call block (lines 52-66) with:

```go
			factsJSON, _ := json.MarshalIndent(chunk, "", "  ")
			data := PromptData{
				Facts:        string(factsJSON),
				RecipePrompt: recipe.Prompt,
				StepPrompt:   step.Prompt,
			}

			systemPrompt, err := RenderTemplate(profile.Name, "prune", "system", data)
			if err != nil {
				return fmt.Errorf("prune: render system: %w", err)
			}
			userPrompt, err := RenderTemplate(profile.Name, "prune", "user", data)
			if err != nil {
				return fmt.Errorf("prune: render user: %w", err)
			}

			opts := llm.CompletionOptions{ForceJSON: profile.ForceJSON}
			response, err := adapter.Complete(ctx, systemPrompt, []llm.Message{{Role: "user", Content: userPrompt}}, opts, nil)
			if err != nil {
				return fmt.Errorf("prune: LLM call %s: %w", label, err)
			}

			result, err := parsePruneResponse(response)
			if err != nil {
				return fmt.Errorf("prune: parse response %s: %w", label, err)
			}

			// Retry if passive and profile says to retry
			if isPrunePassive(result) && profile.RetryOnPassive {
				log.Debug().Str("label", label).Msg("prune: passive response, retrying")
				onProgress(ProgressEvent{Phase: "retry", Message: fmt.Sprintf("prune %s (passive, retrying)", label)})

				retryPrompt, err := RenderTemplate(profile.Name, "prune", "retry", data)
				if err != nil {
					return fmt.Errorf("prune: render retry: %w", err)
				}
				response, err = adapter.Complete(ctx, systemPrompt, []llm.Message{{Role: "user", Content: retryPrompt}}, opts, nil)
				if err != nil {
					return fmt.Errorf("prune: retry LLM call %s: %w", label, err)
				}
				result, err = parsePruneResponse(response)
				if err != nil {
					return fmt.Errorf("prune: retry parse %s: %w", label, err)
				}
				if isPrunePassive(result) {
					log.Warn().Str("label", label).Msg("prune: retry also passive, accepting result")
				}
			}
```

Also add `"encoding/json"` to imports if not already present.

- [ ] **Step 4: Remove buildPrunePrompt and its tests**

Since templates now handle prompt construction, remove `buildPrunePrompt` from `prune_llm.go`. The types (`PruneResult`, `PruneDecision`, etc.), `parsePruneResponse`, `extractJSON`, `chunkFacts`, and `factForLLM` all remain — they're still used.

Also remove the now-dead tests from `synthesize_extra_test.go`:
- `TestBuildPrunePromptWithRecipePrompt`
- `TestBuildPrunePromptWithoutRecipePrompt`

- [ ] **Step 5: Run tests**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/... -v`
Expected: PASS (existing tests should still work since templates produce equivalent output)

- [ ] **Step 6: Commit**

```bash
git add internal/synthesize/pipeline.go internal/synthesize/prune.go internal/synthesize/prune_llm.go
git commit -m "feat(synthesize): wire profile + templates + retry into prune step

Prune now uses profile-specific templates, respects ForceJSON and
MaxChunkBytes from profile, and retries with retry template on
passive responses when RetryOnPassive is true."
```

### Task 13: Update distill step to use profiles, templates, and retry

**Files:**
- Modify: `internal/synthesize/distill.go`
- Modify: `internal/synthesize/distill_llm.go`

- [ ] **Step 1: Update executeDistillStep signature**

In `internal/synthesize/distill.go`, update signature to accept `profile Profile`:

```go
func executeDistillStep(ctx context.Context, gs GitStore, idx SearchIndex, embedder Embedder, adapter llm.LLMAdapter, step RecipeStep, recipe Recipe, profile Profile, onProgress func(ProgressEvent)) error {
```

- [ ] **Step 2: Update runDistillOnGroup to use templates and retry**

In `internal/synthesize/distill_llm.go`, update `runDistillOnGroup` signature to accept `profile Profile`. Replace `const maxChunkBytes = 100_000` with `profile.MaxChunkBytes`. Replace the prompt building + LLM call with template rendering:

```go
func runDistillOnGroup(ctx context.Context, gs GitStore, idx SearchIndex, adapter llm.LLMAdapter, group []factForLLM, step RecipeStep, recipe Recipe, profile Profile, onProgress func(ProgressEvent)) ([]distillFact, []string, error) {
	chunks := chunkFacts(group, profile.MaxChunkBytes)
	...
	for i, chunk := range chunks {
		factsJSON, _ := json.MarshalIndent(chunk, "", "  ")
		data := PromptData{
			Facts:        string(factsJSON),
			RecipePrompt: recipe.Prompt,
			StepPrompt:   step.Prompt,
		}

		systemPrompt, err := RenderTemplate(profile.Name, "distill", "system", data)
		if err != nil {
			return nil, nil, fmt.Errorf("distill: render system: %w", err)
		}
		userPrompt, err := RenderTemplate(profile.Name, "distill", "user", data)
		if err != nil {
			return nil, nil, fmt.Errorf("distill: render user: %w", err)
		}

		opts := llm.CompletionOptions{ForceJSON: profile.ForceJSON}
		response, err := adapter.Complete(ctx, systemPrompt, []llm.Message{{Role: "user", Content: userPrompt}}, opts, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("distill LLM chunk %d: %w", i+1, err)
		}
		result, err := parseDistillResponse(response)
		if err != nil {
			return nil, nil, fmt.Errorf("distill parse chunk %d: %w", i+1, err)
		}

		// Collect input paths for passive detection
		inputPaths := make([]string, len(chunk))
		for j, f := range chunk {
			inputPaths[j] = f.File
		}

		if isDistillPassive(result, inputPaths) && profile.RetryOnPassive {
			log.Debug().Int("chunk", i+1).Msg("distill: passive response, retrying")
			onProgress(ProgressEvent{Phase: "retry", Message: fmt.Sprintf("distill chunk %d (passive, retrying)", i+1)})

			retryPrompt, err := RenderTemplate(profile.Name, "distill", "retry", data)
			if err != nil {
				return nil, nil, fmt.Errorf("distill: render retry: %w", err)
			}
			response, err = adapter.Complete(ctx, systemPrompt, []llm.Message{{Role: "user", Content: retryPrompt}}, opts, nil)
			if err != nil {
				return nil, nil, fmt.Errorf("distill retry chunk %d: %w", i+1, err)
			}
			result, err = parseDistillResponse(response)
			if err != nil {
				return nil, nil, fmt.Errorf("distill retry parse chunk %d: %w", i+1, err)
			}
			if isDistillPassive(result, inputPaths) {
				log.Warn().Int("chunk", i+1).Msg("distill: retry also passive, accepting result")
			}
		}
		...
	}
```

- [ ] **Step 3: Update all callers of runDistillOnGroup in distill.go**

In `distill.go`, pass `profile` to all `runDistillOnGroup` calls (lines 86, 105):

```go
synthesized, forget, err := runDistillOnGroup(ctx, gs, idx, adapter, group, step, recipe, profile, onProgress)
```

- [ ] **Step 4: Remove buildDistillPrompt and its tests**

`buildDistillPrompt` is now replaced by templates. Remove it from `distill_llm.go`.

Also remove the now-dead tests from `synthesize_extra_test.go`:

- `TestBuildDistillPromptWithRecipePrompt`
- `TestBuildDistillPromptWithoutRecipePrompt`

- [ ] **Step 5: Run all tests**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/... -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/synthesize/distill.go internal/synthesize/distill_llm.go
git commit -m "feat(synthesize): wire profile + templates + retry into distill step

Distill now uses profile-specific templates, respects ForceJSON and
MaxChunkBytes from profile, and retries on passive responses."
```

### Task 14: Update existing tests + add retry integration test

**Files:**
- Modify: `internal/synthesize/prune_test.go`
- Modify: `internal/synthesize/pipeline_test.go`
- Modify: `internal/synthesize/synthesize_extra_test.go`

- [ ] **Step 1: Update prune_test.go for new signatures**

Update `executePruneStep` calls to include the profile argument. Add `adapter.EXPECT().Model().Return("claude-sonnet-4-20250514").AnyTimes()` where needed. Update `Complete` mock expectations to match 5 args.

- [ ] **Step 2: Update pipeline_test.go for new signatures**

Add `adapter.EXPECT().Model().Return("claude-sonnet-4-20250514").AnyTimes()` to every test that creates a `MockLLMAdapter`. This is needed because `Run()` calls `resolveStepProfile(step, adapter)` which calls `adapter.Model()` before dispatching to the step executor. Affected tests (check all, list may not be exhaustive):
- `TestRunPruneOnly`
- `TestRunDistillNoEmbeddings`
- `TestRunDistillWithFacts`
- `TestRunNilProgress`

Also update any `DoAndReturn` closures to the 5-parameter signature.

- [ ] **Step 3: Add retry integration test**

Add to an appropriate test file:

```go
func TestPruneStep_RetryOnPassive(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)

	files := map[string]string{
		"know/test/foo.md": factContent("Foo", "Foo body"),
		"know/test/bar.md": factContent("Bar", "Bar body"),
	}

	gs.EXPECT().ListAll().Return([]string{"know/test/foo.md", "know/test/bar.md"}, nil)
	gs.EXPECT().ReadFile(gomock.Any()).DoAndReturn(func(path string) (string, error) {
		if c, ok := files[path]; ok {
			return c, nil
		}
		return "", fmt.Errorf("not found: %s", path)
	}).AnyTimes()

	idx.EXPECT().ClusterFacts(gomock.Any(), gomock.Any()).Return(store.ClusterResult{Clusters: map[int][]string{}}, nil)

	// First call: passive (all keep)
	passiveResponse := `{"decisions": [{"path": "know/test/foo.md", "action": "keep"}, {"path": "know/test/bar.md", "action": "keep"}], "merges": []}`
	// Second call (retry): active (forget bar)
	activeResponse := `{"decisions": [{"path": "know/test/foo.md", "action": "keep"}, {"path": "know/test/bar.md", "action": "forget"}], "merges": []}`

	gomock.InOrder(
		adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(passiveResponse, nil),
		adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(activeResponse, nil),
	)

	gs.EXPECT().DeleteFile("know/test/bar.md", gomock.Any()).Return(nil)
	idx.EXPECT().Delete("know/test/bar.md").Return(nil)
	gs.EXPECT().Tag(gomock.Any()).Return(nil)

	profile := Profile{Name: "small", ForceJSON: false, RetryOnPassive: true, MaxChunkBytes: 100_000}
	step := RecipeStep{Mode: "prune"}
	recipe := Recipe{Name: "test"}

	err := executePruneStep(context.Background(), gs, idx, nil, adapter, step, recipe, profile, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("executePruneStep: %v", err)
	}
}
```

- [ ] **Step 4: Run all tests**

Run: `cd /Users/knomit/data/mine/knomit && go test ./internal/synthesize/... -v`
Expected: PASS

- [ ] **Step 5: Run full project tests**

Run: `cd /Users/knomit/data/mine/knomit && go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/synthesize/prune_test.go internal/synthesize/pipeline_test.go internal/synthesize/synthesize_extra_test.go
git commit -m "test(synthesize): update tests for profile system and add retry test

Update mock expectations for new Complete/Model signatures.
Add integration test verifying retry-on-passive flow."
```

### Task 15: Final verification and cleanup

- [ ] **Step 1: Run full test suite**

Run: `cd /Users/knomit/data/mine/knomit && go test ./...`
Expected: all PASS

- [ ] **Step 2: Run go vet**

Run: `cd /Users/knomit/data/mine/knomit && go vet ./...`
Expected: no issues

- [ ] **Step 3: Verify no dead code**

Check that `buildPrunePrompt` and `buildDistillPrompt` have been removed. If any unused imports remain, clean them up.

- [ ] **Step 4: Final commit if any cleanup needed**

```bash
git add -u
git commit -m "chore(synthesize): remove dead prompt builders and unused imports"
```
