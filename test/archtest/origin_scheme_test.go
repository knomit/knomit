package archtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fleet transport's go-git scheme is INTERNAL (user ruling, 2026-10-15:
// "what's the benefit of this crappy new invention?"). A user types a plain
// https:// URL for another knomit instance; knomit classifies it once and
// stores auth method cert, and only pki.GitURL derives the internal form, for
// go-git and the derived .git/config. So no source outside internal/foundation/pki — no
// API, no UI, no openapi, no test that builds a URL by hand — spells the
// scheme: tests that need the internal form derive it with pki.GitURL.
//
// The needle is assembled so this file does not match itself.
var internalScheme = "knomit" + "+https"

func TestInternalFleetSchemeLivesOnlyInPKI(t *testing.T) {
	root := moduleRoot(t)
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			name := d.Name()
			if rel != "." && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "dist" || name == "third_party") {
				return filepath.SkipDir
			}
			if rel == "internal/foundation/pki" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".ts", ".tsx", ".yaml", ".yml":
		default:
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(b)), internalScheme) {
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("the internal fleet scheme is spelled outside internal/foundation/pki in %v: name a fleet peer by its https URL, and derive the internal form with pki.GitURL", hits)
	}
}
