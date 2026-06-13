package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"knomit/internal/config"
	"knomit/internal/embeddings"
)

// warmModelsCmd builds the `knomit warm-models` subcommand. It downloads the
// configured embedding model's files into <home>/models/<id>/ so a later run
// (e.g. in a container) starts with the model already present and performs no
// network downloads. Used at Docker image build time to bake the model in.
//
// It deliberately does NOT boot the app or initialise ONNX Runtime — it only
// fetches files via embeddings.EnsureModel — so it can run in a build stage
// without the ORT shared library present.
func warmModelsCmd() *cobra.Command {
	var modelOverride string
	cmd := &cobra.Command{
		Use:   "warm-models",
		Short: "Pre-download the embedding model into the cache (no server, no ORT init)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			id := cfg.Embeddings.Model
			if modelOverride != "" {
				id = modelOverride
			}
			model, err := embeddings.Lookup(id)
			if err != nil {
				return fmt.Errorf("unknown embedding model %q: %w", id, err)
			}
			cacheDir := filepath.Join(cfg.Home, "models")
			modelPath, tokPath, err := embeddings.EnsureModel(model, cacheDir)
			if err != nil {
				return fmt.Errorf("download model %q: %w", id, err)
			}
			fmt.Printf("model %q ready:\n  %s\n  %s\n", id, modelPath, tokPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&modelOverride, "model", "", "embedding model id to fetch (default: from config)")
	return cmd
}
