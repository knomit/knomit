# Profile-Driven Prompt Library for Synthesize Pipeline

## Problem

When using Ollama with small models (e.g. Qwen3:8b), both synthesize prune and distill produce no-op results: everything is kept, nothing is learned. Root causes:

1. `format: "json"` kills chain-of-thought reasoning in small models
2. Prompts are too abstract — no examples, no explicit decision criteria
3. No detection or recovery when a model returns a passive response

## Design

### 1. Profile System

Two capability profiles: `large` and `small`.

```go
// internal/synthesize/profile.go

type Profile struct {
    Name           string  // "large" or "small"
    ForceJSON      bool    // whether to set format:"json" on the adapter
    RetryOnPassive bool    // retry with sharper prompt if response is passive
    MaxChunkBytes  int     // max facts payload size per LLM call (replaces hardcoded 100_000)
}
```

**Resolution order (per step):**

1. Step-level `profile:` field (explicit override)
2. Auto-detect via `ResolveProfile(model string) Profile` using size markers in model name (`8b`, `7b`, `3b` → small; everything else → large)

**LLMAdapter gains `Model() string`** so the pipeline can resolve profile without threading model names through config.

**RecipeStep YAML additions:**
```yaml
steps:
  - mode: prune
    profile: small           # optional override (auto-detected if omitted)
    retry_on_passive: false   # optional override (default from profile)
```

Profile and retry settings live on `RecipeStep`, not `Recipe`, because different steps can use different models (via the existing `model` field on `RecipeStep`).

### 2. Prompt Templates

Embedded `.txt` files under `internal/synthesize/prompts/`:

```
prompts/
  large/
    prune_system.txt
    prune_user.txt
    prune_retry.txt
    distill_system.txt
    distill_user.txt
    distill_retry.txt
  small/
    prune_system.txt
    prune_user.txt
    prune_retry.txt
    distill_system.txt
    distill_user.txt
    distill_retry.txt
```

Templates use Go `text/template` with variables: `{{.Facts}}`, `{{.RecipePrompt}}`, `{{.StepPrompt}}`.

**Large profile** templates keep the current abstract style — they work well with capable models.

**Small profile** templates include:
- Few-shot JSON examples showing exact expected output format
- Explicit decision criteria (e.g. "if two facts say the same thing, mark one as forget")
- Shorter, more direct language
- A `<think>` preamble hint so the model reasons before emitting JSON (enabled by not forcing JSON mode). The existing `extractJSON` function is extended to strip `<think>...</think>` blocks before locating the JSON payload.

### 3. LLMAdapter Interface Changes

```go
type LLMAdapter interface {
    Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error)
    Model() string
}

type CompletionOptions struct {
    ForceJSON bool
}
```

- `Model()` returns the model name string for profile auto-detection.
- `CompletionOptions` replaces implicit JSON forcing. `ForceJSON` is set from the profile.
- Ollama adapter reads `opts.ForceJSON` instead of unconditionally setting `Format: "json"`.
- Anthropic adapter ignores `ForceJSON`.
- All existing callers pass `CompletionOptions{}` (zero value = no JSON forcing).

### 4. Validation + Retry

**Passive detection** (pure functions, no LLM):
- Prune passive: every fact marked `"keep"`, zero `"forget"`, zero `"merge"`.
- Distill passive: empty `synthesize` array, or `forget` array is empty while all synthesized fact paths match input paths (i.e. no actual synthesis happened).

**Flow:**
1. First call uses `user.txt` template.
2. If passive **and** `profile.RetryOnPassive` is `true`, re-call with `retry.txt` — a blunter prompt.
3. If retry is also passive, accept result and log warning. No infinite loops.

**Configuration:**
- `RetryOnPassive` defaults to `false`.
- `small` profile sets it to `true`.
- Step-level `retry_on_passive:` override available.

### 5. Integration & Data Flow

```
For each step:
  ├─ step.Profile override? → use it
  └─ no override → adapter.Model() → ResolveProfile(model) → profile

  1. profile + step → load templates (system.txt, user.txt, retry.txt)
  2. render templates with {Facts, RecipePrompt, StepPrompt}
  3. build CompletionOptions{ForceJSON: profile.ForceJSON}
  4. adapter.Complete(ctx, system, msgs, opts, onChunk)
  5. parse response (same JSON schema for both profiles)
  6. if passive && profile.RetryOnPassive:
       render retry.txt → adapter.Complete again
       re-parse
  7. apply results (write/delete facts, update index)
```

- `ResolveProfile` lives in `internal/synthesize/profile.go`.
- Templates embedded via `//go:embed` in a single `prompts.go` file.
- Pipeline passes `profile` to `executePruneStep` and `executeDistillStep` — no global state.
- JSON parsing is identical for both profiles — only prompts differ.
- The `learn` MCP handler doesn't use LLM today; same profile system applies if it does later.

### 6. Testing

- **`ResolveProfile` unit tests:** table-driven, model name → expected profile.
- **Template rendering tests:** load template, render with sample data, verify expected markers.
- **Passive detection tests:** `isPrunePassive` and `isDistillPassive` with known inputs.
- **Retry flow integration test:** mock adapter returns passive first, active second; verify retry template used.
- All new tests use uber-go/mock per project conventions.

## Files to Create/Modify

**New files:**
- `internal/synthesize/profile.go` — Profile struct + ResolveProfile
- `internal/synthesize/profile_test.go`
- `internal/synthesize/prompts.go` — `//go:embed` + template loading/rendering
- `internal/synthesize/prompts_test.go`
- `internal/synthesize/validation.go` — passive detection functions
- `internal/synthesize/validation_test.go`
- `internal/synthesize/prompts/large/*.txt` (6 files)
- `internal/synthesize/prompts/small/*.txt` (6 files)

**Modified files:**
- `internal/llm/adapter.go` — add `CompletionOptions`, `Model()` to interface
- `internal/llm/ollama.go` — read `opts.ForceJSON`, implement `Model()`
- `internal/llm/anthropic.go` — implement `Model()`, pass-through opts
- `internal/llm/bedrock.go` — implement `Model()`, pass-through opts
- `internal/llm/gemini.go` — implement `Model()`, pass-through opts
- `internal/llm/claudecli.go` — implement `Model()`, pass-through opts
- `internal/llm/geminicli.go` — implement `Model()`, pass-through opts
- `internal/llm/resolver.go` — verify factory still works (no structural change expected)
- `internal/llm/mock_adapter_test.go` — regenerate (mockgen)
- `internal/llm/anthropic_test.go` — update `Complete` call signatures
- `internal/llm/ollama_test.go` — update `Complete` call signatures
- `internal/llm/llm_extra_test.go` — update `Complete` call signatures
- `internal/synthesize/recipe.go` — add `Profile`, `RetryOnPassive` fields to RecipeStep
- `internal/synthesize/pipeline.go` — resolve profile per step, thread it through
- `internal/synthesize/prune.go` — use templates + validation + retry
- `internal/synthesize/prune_llm.go` — extract current prompts to large templates, extend `extractJSON` for `<think>` blocks
- `internal/synthesize/distill.go` — use templates + validation + retry
- `internal/synthesize/distill_llm.go` — extract current prompts to large templates
