// Command calibrate measures the cosine-similarity geometry of one or more
// embedding models against a real knomit corpus, so the model-dependent
// retrieval thresholds (dedup, SIMILAR_TO, search recall floor, rerank tiers,
// reflect novelty) can be re-derived when the default embedding model changes.
//
// The thresholds are absolute points on a cosine distribution, and that
// distribution is model-specific. When the default model changes, each
// threshold is ported by PRESERVING THE PERCENTILE it occupied on the baseline
// model's distribution: for a baseline value T on distribution D, find
// p = CDF_baseline(T), then the new model's value is the p-quantile of its own
// D. This keeps each gate's selectivity stable across the model swap without
// needing labeled relevance judgments.
//
// Usage:
//
//	ORT_LIB_PATH=dist/darwin-arm64/lib/libonnxruntime.dylib DYLD_LIBRARY_PATH=dist/darwin-arm64/lib \
//	  go run ./tools/calibrate embeddings --cache ~/.knomit/models \
//	    ~/.knomit/repos/trunk.db ~/.knomit/repos/knomit-kb.db
//
// Subcommands:
//
//	embeddings    measure distributions and port thresholds
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "calibrate <subcommand>",
		Short: "Calibrate knomit model-dependent retrieval thresholds",
		Long: `calibrate measures the cosine-similarity geometry of embedding models against
a real knomit corpus so that model-dependent retrieval thresholds can be
re-derived when the default embedding model changes.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.AddCommand(newEmbeddingsCmd())
	return cmd
}
