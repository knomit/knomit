// Command corpusgen builds real, standalone knomit knowledge-base repos at a
// deliberately chosen size and topic-diversity profile, through the actual
// write/embed path (store.Service, a real ONNX embedder) rather than mocks
// or in-memory fixtures. It exists to unblock bridge/YAKE calibration work
// that, until now, only ever ran against whatever the live corpus happened
// to contain — see .claude/plans/yake-*.md for the research this supports.
//
// Usage:
//
//	ORT_LIB_PATH=dist/lib/libonnxruntime.dylib DYLD_LIBRARY_PATH=dist/lib \
//	  go run ./tools/corpusgen build --out ~/knomit-corpora/narrow-100 \
//	    --size 100 --diversity narrow --ontology code --topic architecture
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
		Use:   "corpusgen <subcommand>",
		Short: "Generate real knomit knowledge-base repos for bridge/YAKE calibration",
		Long: `corpusgen builds standalone knomit repos through the real write/embed path
(not mocks) at a chosen size and topic-diversity profile, so bridge-discovery
and YAKE keyword-extraction tuning can be tested against corpora built for
the purpose instead of whatever the live KB happens to contain.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.AddCommand(newBuildCmd())
	cmd.AddCommand(newRegisterCmd())
	return cmd
}
