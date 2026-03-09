# CLI-Based LLM Adapters Design

## Problem

The current synthesis engine requires API keys (`ANTHROPIC_API_KEY`, `GOOGLE_AI_API_KEY`, or AWS credentials) to call LLMs. This excludes the majority of potential users — people with **Anthropic Max subscriptions** or **Google AI Pro subscriptions** who have access to LLMs through CLI tools but don't have (or want to manage) API keys.

## Proposal

Add two new LLM adapter implementations that shell out to locally-installed CLI tools instead of making direct HTTP API calls:

- **`claude-cli`** — Uses the `claude` CLI (Claude Code) in print mode
- **`gemini-cli`** — Uses the `gemini` CLI (Gemini CLI) in non-interactive mode

These adapters implement the same `LLMAdapter` interface as the existing API-based adapters, so the rest of the synthesis engine is unchanged.

## CLI Tool Capabilities

### Claude Code (`claude`)

```bash
claude -p --system "system prompt" --output-format text "user prompt"
# or via stdin:
echo "user prompt" | claude -p --system "system prompt" --output-format text
```

- Ships with Anthropic Max subscriptions (no API key needed)
- `-p` / `--print` mode: non-interactive, outputs response to stdout
- `--system` flag for system prompts
- `--output-format text` for raw text (no markdown wrapping)
- Streams tokens to stdout as they are generated

### Gemini CLI (`gemini`)

```bash
echo "user prompt" | gemini
```

- Ships with Google AI Pro subscriptions (no API key needed)
- Reads from stdin in pipe mode
- System prompt must be prepended to user message
- Streams tokens to stdout as they are generated

## Why Streaming Matters

With thousands of facts, synthesis produces **10-20+ LLM calls**, each potentially running 30-60+ seconds and generating large JSON responses (hundreds of prune decisions or distill results per chunk).

Without streaming the subprocess stdout:

1. **Dead air** — The user sees nothing for minutes per chunk. The progress event system only fires *between* LLM calls; within a single call there is no feedback.
2. **Buffer pressure** — A 60s+ generation producing hundreds of KB of JSON can fill OS pipe buffers (typically 64KB on Linux). If the parent process isn't draining stdout, the child process blocks on write and hangs.
3. **Late failure detection** — Error messages, malformed output, or model refusals aren't visible until after the full process exits.

We can't parse partial JSON, so streaming doesn't enable incremental *processing*. But it enables:

- **Liveness signaling** — Show a byte counter or spinner so the user knows the LLM is working ("receiving... 48KB")
- **Buffer draining** — Continuously read stdout to prevent pipe deadlocks
- **Early abort** — Detect errors or empty output without waiting for process completion
- **Verbose mode** — In `--verbose`, show raw LLM tokens as they arrive (useful for debugging prompts)

## Adapter Implementation

### Interface Extension

The `LLMAdapter` interface stays the same. Streaming progress is communicated through an optional callback on the adapter instance, not through the interface contract:

```ts
interface CliAdapterOptions {
  onChunk?: (text: string) => void;  // called as stdout is drained
}
```

### Claude CLI Adapter

```ts
function createClaudeCliAdapter(options?: CliAdapterOptions): LLMAdapter {
  return {
    async complete(system: string, messages: Message[]): Promise<string> {
      const userContent = messages
        .filter(m => m.role === "user")
        .map(m => m.content)
        .join("\n\n");

      const proc = Bun.spawn(
        ["claude", "-p", "--system", system, "--output-format", "text"],
        { stdin: new Blob([userContent]) }
      );

      const chunks: string[] = [];
      const reader = proc.stdout.getReader();
      const decoder = new TextDecoder();

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        const text = decoder.decode(value, { stream: true });
        chunks.push(text);
        options?.onChunk?.(text);
      }

      const exitCode = await proc.exited;
      if (exitCode !== 0) {
        const stderr = await new Response(proc.stderr).text();
        throw new Error(`claude CLI exited with code ${exitCode}: ${stderr}`);
      }

      return chunks.join("");
    },
  };
}
```

### Gemini CLI Adapter

```ts
function createGeminiCliAdapter(options?: CliAdapterOptions): LLMAdapter {
  return {
    async complete(system: string, messages: Message[]): Promise<string> {
      // Gemini CLI has no --system flag; prepend to user content
      const userContent = messages
        .filter(m => m.role === "user")
        .map(m => m.content)
        .join("\n\n");
      const fullPrompt = `${system}\n\n${userContent}`;

      const proc = Bun.spawn(["gemini"], {
        stdin: new Blob([fullPrompt]),
      });

      const chunks: string[] = [];
      const reader = proc.stdout.getReader();
      const decoder = new TextDecoder();

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        const text = decoder.decode(value, { stream: true });
        chunks.push(text);
        options?.onChunk?.(text);
      }

      const exitCode = await proc.exited;
      if (exitCode !== 0) {
        const stderr = await new Response(proc.stderr).text();
        throw new Error(`gemini CLI exited with code ${exitCode}: ${stderr}`);
      }

      return chunks.join("");
    },
  };
}
```

## Provider Resolution

Extend `resolveProvider` to support the new provider types:

```ts
type Provider = "anthropic" | "gemini" | "bedrock" | "claude-cli" | "gemini-cli";
```

### Auto-detection fallback

When no API key is available for a provider, check if the corresponding CLI tool is installed and fall back automatically:

```ts
function resolveProvider(model: string, explicit?: string): Provider {
  // Explicit provider always wins
  if (explicit) return explicit;

  // Model-based inference (existing logic)
  if (model.startsWith("claude")) {
    // Try API first, fall back to CLI
    if (process.env.ANTHROPIC_API_KEY) return "anthropic";
    if (cliExists("claude")) return "claude-cli";
    throw new Error("No ANTHROPIC_API_KEY and 'claude' CLI not found");
  }
  if (model.startsWith("gemini")) {
    if (process.env.GOOGLE_AI_API_KEY) return "gemini";
    if (cliExists("gemini")) return "gemini-cli";
    throw new Error("No GOOGLE_AI_API_KEY and 'gemini' CLI not found");
  }
  // ... bedrock unchanged
}

function cliExists(name: string): boolean {
  try {
    const result = Bun.spawnSync(["which", name]);
    return result.exitCode === 0;
  } catch {
    return false;
  }
}
```

This means existing API-key users see zero behavior change. New users without keys get automatic CLI fallback.

## Progress Event Extension

Add a new progress event for streaming visibility:

```ts
| { phase: "llm-stream"; step: number; totalSteps: number; bytes: number }
```

The CLI renderer in `src/cli/synthesize.ts` can show a byte counter that updates in-place:

```
  Sending 847 facts to LLM [prune]...
  Receiving... 128KB
```

## Configuration

### Environment Variables

```
KNOMIT_LLM_PROVIDER=claude-cli   # Explicit CLI mode
KNOMIT_LLM_PROVIDER=gemini-cli   # Explicit CLI mode
# Or omit — auto-detection will find the CLI if no API key is set
```

### Recipe Model Override

CLI adapters ignore the `model` field in recipe steps — the CLI tool uses whatever model the user's subscription provides. This is acceptable because:

- Max users get the best available Claude model
- Google AI Pro users get the best available Gemini model
- The synthesis prompts are model-agnostic (they request JSON output, which all current models handle)

If a recipe specifies `model: gemini-2.0-flash` but the provider resolves to `claude-cli`, the model field is ignored with a warning log.

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

## CLI Validation

Update `src/cli/synthesize.ts` to handle the keyless path:

```ts
// Current: fails if no API key
if (provider === "anthropic" && !config.apiKey) {
  console.error("Error: ANTHROPIC_API_KEY is required.");
  process.exit(1);
}

// New: only fail if no API key AND no CLI
if (provider === "anthropic" && !config.apiKey) {
  if (!cliExists("claude")) {
    console.error("Error: ANTHROPIC_API_KEY is required, or install Claude Code CLI.");
    process.exit(1);
  }
  // Will auto-resolve to claude-cli at runtime
}
```

## Open Questions

1. **Multi-turn conversations** — The current `LLMAdapter.complete()` accepts a `messages` array, but CLI tools work best with single-turn prompts. The synthesis engine currently only sends a single user message, so this isn't an issue today. If multi-turn is needed later, we'd need to either serialize the conversation into a single prompt or use the CLI tools' conversation features.

2. **Token/context limits** — API adapters let us set `max_tokens: 8192`. CLI tools use their own defaults. For large synthesis responses, we may need to investigate whether CLI defaults are sufficient or if there are flags to increase output length.

3. **Structured output** — Some CLI tools may wrap output in markdown code fences or add preamble text. The existing `extractJson()` parser already handles markdown-wrapped JSON, but we should test edge cases with both CLIs.

4. **Stderr noise** — CLI tools may print progress indicators, warnings, or update notices to stderr. The adapter should capture stderr separately for error reporting but not mix it into the response.

5. **Concurrent calls** — Synthesis runs multiple chunks sequentially today, but future parallelization would spawn multiple CLI processes. Need to verify that subscription rate limits allow this.
