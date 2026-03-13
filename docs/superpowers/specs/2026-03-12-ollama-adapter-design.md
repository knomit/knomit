# Ollama LLM Adapter

## Summary

Add an Ollama provider adapter to knomit, enabling local LLM inference via Ollama's REST API for learning, pruning, and distillation operations. Uses a direct HTTP implementation (no third-party SDK) with streaming and JSON mode support.

**Target model:** `qwen3:8b` (fits comfortably on M4/32GB, good JSON output quality).

## Architecture

### New File: `internal/llm/ollama.go`

Implements the existing `LLMAdapter` interface:

```go
type OllamaAdapter struct {
    host  string
    model string
    client *http.Client
}
```

**Constructor:** `NewOllamaAdapter(host, model string) (*OllamaAdapter, error)`
- Validates connectivity via `GET /api/tags`
- Returns error if Ollama is unreachable

**Complete():** `POST /api/chat`

Request body:

```json
{
  "model": "qwen3:8b",
  "messages": [
    {"role": "system", "content": "..."},
    {"role": "user", "content": "..."},
    {"role": "assistant", "content": "..."}
  ],
  "format": "json",
  "stream": true,
  "options": {
    "num_predict": 8192
  }
}
```

- Full multi-turn support (system + user + assistant messages)
- `format: "json"` enforces valid JSON output from Ollama
- `stream: true` with line-delimited JSON responses
- `options.num_predict` set to `defaultMaxTokens` (8192) for consistency with other adapters

**Streaming protocol:** Each line of the response is a JSON object:

```json
{"message": {"role": "assistant", "content": "chunk"}, "done": false}
```

Final line has `"done": true`. The adapter reads line by line, calls `onChunk` with content deltas, and accumulates the full response.

**Error handling:**
- Non-2xx HTTP status: parse Ollama error response body, return as error
- Connection refused: clear error message pointing to Ollama not running
- Context cancellation: respected via `ctx` on the HTTP request

### Config Changes: `internal/config/config.go`

New env var:
- `OLLAMA_HOST` — Ollama server URL, default `http://localhost:11434`

The host value is passed through to `NewOllamaAdapter`.

### Resolver Changes: `internal/llm/resolver.go`

- Add `"ollama"` as a recognized provider in `NewAdapter()`
- **No auto-detection** — requires explicit `KNOMIT_LLM_PROVIDER=ollama`
- New case in adapter construction: `"ollama"` -> `NewOllamaAdapter(host, model)`

### Usage

```sh
# Local Ollama (default host)
KNOMIT_LLM_PROVIDER=ollama KNOMIT_LLM_MODEL=qwen3:8b knomit

# Remote Ollama instance
OLLAMA_HOST=http://192.168.1.50:11434 KNOMIT_LLM_PROVIDER=ollama KNOMIT_LLM_MODEL=qwen3:8b knomit
```

## Testing

### New File: `internal/llm/ollama_test.go`

Uses `uber-go/mock` (mockgen) for all mocks — no hand-rolled mocks.

Uses `httptest.NewServer` to mock the Ollama API:

- **Request construction:** Verify correct JSON body structure (model, messages, format, stream, options)
- **Message mapping:** System prompt becomes system message; user/assistant messages map correctly with multi-turn support
- **Streaming parsing:** Mock multi-line streaming response, verify chunk callback invocation and final accumulation
- **Done detection:** Verify streaming terminates correctly on `"done": true`
- **Error cases:**
  - Non-2xx HTTP status returns meaningful error
  - Malformed JSON in stream is handled gracefully
  - Connection refused produces clear error message
- **JSON mode:** Verify `format: "json"` is always set in requests

### Integration Coverage

The existing `LLMAdapter` interface means the Ollama adapter is automatically usable by the synthesis pipeline (`internal/synthesize/`). No changes needed to distill/prune code.

## Dependencies

- No new third-party dependencies (uses `net/http`, `encoding/json` from stdlib)
- `uber-go/mock` added as test dependency (if not already present)

## Files Changed

| File | Change |
|------|--------|
| `internal/llm/ollama.go` | New — adapter implementation |
| `internal/llm/ollama_test.go` | New — unit tests |
| `internal/llm/resolver.go` | Add `"ollama"` provider case |
| `internal/config/config.go` | Add `OLLAMA_HOST` env var |
| `cmd/knomit/main.go` | Pass `OLLAMA_HOST` to adapter construction |
