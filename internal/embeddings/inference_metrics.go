package embeddings

import (
	"time"

	"knomit/internal/obs/metrics"
)

// embedInferenceSeconds times each ONNX inference batch (session.Run). Buckets
// span sub-millisecond to multi-second since latency scales with batch size and
// model. It is the highest-value latency signal for this system — embedding
// runs on essentially every fact write.
var embedInferenceSeconds = metrics.Default().Histogram(
	"knomit_embed_inference_seconds",
	"ONNX embedding inference latency per batch, in seconds.",
	[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
)

// observeEmbedInference records the elapsed time since start as one inference
// observation.
func observeEmbedInference(start time.Time) {
	embedInferenceSeconds.Observe(time.Since(start).Seconds())
}
