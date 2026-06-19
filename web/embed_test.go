//go:build !noembed

package webui

import (
	"io/fs"
	"testing"
)

// TestEmbed_IncludesGitkeep guards the `all:` prefix on the //go:embed directive.
//
// The build-generated dist assets are gitignored, so a fresh checkout (and CI,
// which runs `go build` without `make web`) has only the committed `.gitkeep`
// in web/dist. Plain `//go:embed dist` silently excludes dot-prefixed files,
// so that directory embeds nothing and the build fails with "contains no
// embeddable files". Asserting `.gitkeep` is present in the embedded FS proves
// `all:dist` is in effect; if someone drops the `all:` prefix this test fails
// locally (where dist has real assets and so still compiles) before CI breaks.
func TestEmbed_IncludesGitkeep(t *testing.T) {
	sub, err := FS()
	if err != nil {
		t.Fatalf("FS(): %v", err)
	}
	if _, err := fs.Stat(sub, ".gitkeep"); err != nil {
		t.Fatalf("embedded dist is missing .gitkeep — is //go:embed using `all:dist`? %v", err)
	}
}
