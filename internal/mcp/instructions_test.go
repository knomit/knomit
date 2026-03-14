package mcp

import (
	"strings"

	"knomit/internal/fact"
	"testing"
)

func TestProfileInstructions(t *testing.T) {
	for _, p := range []string{"code", "chat", "generic"} {
		got := ProfileInstructions(p, "general", fact.DefaultOntology())
		if got == "" {
			t.Fatalf("ProfileInstructions(%q) returned empty string", p)
		}
	}

	code := ProfileInstructions("code", "general", fact.DefaultOntology())
	unknown := ProfileInstructions("unknown", "general", fact.DefaultOntology())
	if code != unknown {
		t.Fatal("unknown profile should fall back to code")
	}

	chat := ProfileInstructions("chat", "general", fact.DefaultOntology())
	if code == chat {
		t.Fatal("code and chat profiles should differ")
	}
}

func TestProfileInstructionsCustomOntologyRoot(t *testing.T) {
	text := ProfileInstructions("code", "facts", fact.DefaultOntology())
	if !strings.Contains(text, "facts/") {
		t.Errorf("ProfileInstructions should interpolate ontologyRoot, got: %s", text)
	}
	if strings.Contains(text, "general/") {
		t.Errorf("ProfileInstructions should not contain hardcoded 'general/', got: %s", text)
	}
}
