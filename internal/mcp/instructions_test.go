package mcp

import (
	"strings"

	"knomit/internal/fact"
	"testing"
)

func TestProfileInstructions(t *testing.T) {
	for _, p := range []string{"code", "chat", "generic"} {
		got := ProfileInstructions(p, "kb", fact.DefaultOntology())
		if got == "" {
			t.Fatalf("ProfileInstructions(%q) returned empty string", p)
		}
	}

	code := ProfileInstructions("code", "kb", fact.DefaultOntology())
	unknown := ProfileInstructions("unknown", "kb", fact.DefaultOntology())
	if code != unknown {
		t.Fatal("unknown profile should fall back to code")
	}

	chat := ProfileInstructions("chat", "kb", fact.DefaultOntology())
	if code == chat {
		t.Fatal("code and chat profiles should differ")
	}
}

func TestProfileInstructionsCustomOntologyRoot(t *testing.T) {
	text := ProfileInstructions("code", "facts", fact.DefaultOntology())
	if !strings.Contains(text, "facts/") {
		t.Errorf("ProfileInstructions should interpolate ontologyRoot, got: %s", text)
	}
	if strings.Contains(text, "kb/") {
		t.Errorf("ProfileInstructions should not contain hardcoded 'kb/', got: %s", text)
	}
}
