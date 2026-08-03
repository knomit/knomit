package app

import (
	"context"
	"strings"
	"testing"

	"knomit/internal/config"
)

// TestNew_EmbedderRequired regresses the mandatory-embeddings invariant: if no
// embedder can be built (here, an unknown model id), app.New must FAIL rather
// than boot a degraded service that writes vectorless facts and mis-tunes
// params. The error surfaces to cmd/serve's RunE → non-zero exit.
func TestNew_EmbedderRequired(t *testing.T) {
	cfg := config.Config{Home: t.TempDir()}
	cfg.Embeddings.Model = "no-such-model"

	boot, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	a, err := New(context.Background(), cfg, boot, Options{})
	if err == nil {
		if a != nil {
			a.Close()
		}
		t.Fatal("New must fail when no embedder can be built (embeddings are mandatory)")
	}
	if !strings.Contains(err.Error(), "embedder") {
		t.Errorf("error %q should mention the embedder as the failure cause", err.Error())
	}
}
