package app

import (
	"context"
	"testing"

	"knomit/internal/config"
	"knomit/test/testenv"
)

// TestNew_MakesTheDataRootPrivate pins app.New's privdir.Ensure call, the
// backstop every binary that boots through here relies on (knomit#301). It
// boots the production wiring the way the other wiring tests do, through
// Options.Embedder, on a root that is NOT private beforehand (wideHome, per
// OS), and asserts it is private afterwards.
func TestNew_MakesTheDataRootPrivate(t *testing.T) {
	cfg := config.Defaults()
	cfg.Home = wideHome(t)

	a, err := New(context.Background(), cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer a.Close()

	assertPrivateRoot(t, cfg.Home)
}
