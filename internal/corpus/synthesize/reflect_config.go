package synthesize

import (
	"os"
	"strconv"

	"github.com/rs/zerolog/log"
)

// Defaults for reflect-step server-side guards. These are the rarity-bias
// knobs: keep `propose` capped low and the novelty threshold high so new
// methodologies are rare and unique. Both can be overridden at runtime via
// env if a corpus-calibration pass surfaces a better value.
const (
	defaultReflectProposeCap       = 1
	defaultReflectNoveltyThreshold = 0.85
	envReflectProposeCap           = "KNOMIT_REFLECT_PROPOSE_CAP"
	envReflectNoveltyThreshold     = "KNOMIT_REFLECT_NOVELTY_THRESHOLD"
)

// reflectProposeCap returns the maximum number of new methodologies a
// single reflect response may propose. Defaults to 1; env override
// permitted (typical operator choice would be 0 for the strictest mode).
func reflectProposeCap() int {
	if v := os.Getenv(envReflectProposeCap); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			log.Warn().Str(envReflectProposeCap, v).Msg("invalid; using default")
			return defaultReflectProposeCap
		}
		return n
	}
	return defaultReflectProposeCap
}

// reflectNoveltyThreshold returns the cosine-similarity floor at which a
// proposed methodology is rejected as too similar to an existing one. The
// model-dependent value (modelDefault, from the active embedder's calibrated
// Thresholds) is the baseline; an explicit, valid env override takes precedence.
// modelDefault <= 0 falls back to the historical nomic-era constant.
func reflectNoveltyThreshold(modelDefault float64) float64 {
	if v := os.Getenv(envReflectNoveltyThreshold); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			log.Warn().Str(envReflectNoveltyThreshold, v).Msg("invalid; using model default")
		} else {
			return f
		}
	}
	if modelDefault > 0 {
		return modelDefault
	}
	return defaultReflectNoveltyThreshold
}
