# CLI-Based LLM Adapters Design

## Problem

The current synthesis engine requires API keys (`ANTHROPIC_API_KEY`, `GOOGLE_AI_API_KEY`, or AWS credentials) to call LLMs. This excludes the majority of potential users — people with **Anthropic Max subscriptions** or **Google AI Pro subscriptions** who have access to LLMs through CLI tools but don't have (or want to manage) API keys.

## Proposal

Add two new LLM adapter implementations that shell out to locally-installed CLI tools instead of making direct HTTP API calls:

- **`claude-cli`** — Uses the `claude` CLI (Claude Code) in print mode
- **`gemini-cli`** — Uses the `gemini` CLI (Gemini CLI) in non-interactive mode

These adapters implement the same `LLMAdapter` interface as the existing API-based adapters.

## LLMAdapter Interface Change

Add an optional `onChunk` streaming callback to the interface. This applies to **all** adapters (API and CLI), solving the dead-air problem uniformly:

```ts
interface LLMAdapter {
  complete(system: string, messages: Message[], onChunk?: (text: string) => void): Promise<string>;
}
```

All 5 adapters implement streaming via `onChunk`:

| Adapter | Streaming mechanism |
|---------|-------------------|
| Anthropic API | `stream: true`, parse SSE `content_block_delta` events |
| Gemini API | `streamGenerateContent` endpoint, parse chunked JSON |
| Bedrock API | `invoke-with-response-stream`, parse AWS event-stream chunks |
| Claude CLI | `Bun.spawn`, read stdout chunks from subprocess |
| Gemini CLI | `Bun.spawn`, read stdout chunks from subprocess |

Callers that don't pass `onChunk` get identical behavior to today.

## CLI Tool Capabilities

### Claude Code (`claude`)

```bash
echo "user prompt" | claude -p --system "system prompt" --output-format text
```

- Ships with Anthropic Max subscriptions (no API key needed)
- `-p` / `--print` mode: non-interactive, outputs response to stdout
- `--system` flag for system prompts
- `--output-format text` for raw text (no markdown wrapping)

### Gemini CLI (`gemini`)

```bash
echo "user prompt" | gemini
```

- Ships with Google AI Pro subscriptions (no API key needed)
- Reads from stdin in pipe mode
- System prompt must be prepended to user message (no `--system` flag)

## Provider Resolution

Explicit only — user must set `KNOMIT_LLM_PROVIDER=claude-cli` or `gemini-cli`. No auto-detection fallback.

```ts
type Provider = "anthropic" | "gemini" | "bedrock" | "claude-cli" | "gemini-cli";
```

`resolveProvider` validates the new values. `createAdapter` dispatches to the appropriate factory.

### CLI Validation

At startup, `src/cli/synthesize.ts` validates:
- For `claude-cli`: `which claude` succeeds, else error "Claude Code CLI not found"
- For `gemini-cli`: `which gemini` succeeds, else error "Gemini CLI not found"

### Recipe Model Override

CLI adapters pass the `model` field to the CLI tool via `--model <model>`. Both `claude` and `gemini` CLIs support `--model` for selecting a specific model. If no model is specified in the recipe, the CLI uses its default.

## Progress Event

New progress event for streaming liveness:

```ts
| { phase: "llm-stream"; step: number; totalSteps: number; bytes: number }
```

The synthesize engine passes `onChunk` to `adapter.complete()`, accumulating bytes and emitting `llm-stream` events. The CLI renderer shows `Receiving... 48KB` updating in-place.

## Configuration

```
KNOMIT_LLM_PROVIDER=claude-cli   # Use Claude Code CLI
KNOMIT_LLM_PROVIDER=gemini-cli   # Use Gemini CLI
```

## Trade-offs

| | API Adapters (current) | CLI Adapters (proposed) |
|---|---|---|
| **Auth** | API keys required | Subscription login (no keys) |
| **Latency** | Direct HTTP (~200ms overhead) | Process spawn (~1-2s overhead) |
| **Rate limits** | API tier limits | Subscription tier limits (usually more generous) |
| **Reliability** | Stable versioned API | CLI output format could change between versions |
| **Model control** | Exact model selection | Whatever the subscription provides |
| **Cost** | Per-token billing | Included in subscription |
| **Concurrency** | Easy (HTTP connections) | Each call spawns a process |

The process spawn overhead is negligible — each synthesis LLM call takes 10-60s of thinking time, so 1-2s of spawn overhead is <5%.

## Open Questions (Resolved)

1. **Multi-turn conversations** — Not an issue. Synthesis only sends a single user message today.
2. **Token/context limits** — CLI tools use their own defaults. The existing `extractJson()` parser handles markdown-wrapped JSON.
3. **Stderr noise** — Adapters capture stderr separately for error reporting; it's not mixed into the response.
4. **Auto-detection** — Decided against. Explicit `KNOMIT_LLM_PROVIDER` only.
5. **Streaming** — Added to `LLMAdapter` interface for all adapters, not just CLI.
