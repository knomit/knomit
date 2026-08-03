package llm

import (
	"time"

	"knomit/internal/obs/metrics"
)

// llmRequestSeconds times one *logical* completion — the whole retry loop, not
// each attempt. That is deliberately the caller's view: a request that
// succeeded on its third try cost the caller all three attempts plus the
// backoff between them, and a histogram that hid that would understate the
// latency synthesis actually experiences. The top bucket is sized to that same
// view: a gemini or ollama request may legitimately spend three attempts of
// five minutes each plus backoff (policyFor, resilience.go), so buckets stop at
// 900s. Ending at 600s would drop every fully-retried request into +Inf, which
// is exactly the population the histogram exists to measure.
var llmRequestSeconds = metrics.Default().Histogram(
	"knomit_llm_request_seconds",
	"LLM completion latency per logical request, including retries, in seconds.",
	[]float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 900},
)

// llmRetriesTotal counts retry attempts, not requests: it stays at zero on a
// healthy system, so any sustained rise is the signal that a provider is
// degrading — visible well before the retries stop being enough and errors
// reach callers.
var llmRetriesTotal = metrics.Default().Counter(
	"knomit_llm_retries_total",
	"Total LLM completion attempts retried after a retryable failure.",
)

// observeLLMRequest records the elapsed time since start as one request
// observation.
func observeLLMRequest(start time.Time) {
	llmRequestSeconds.Observe(time.Since(start).Seconds())
}
