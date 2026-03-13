package mcp

import (
	"strings"
	"testing"
)

func TestProfileInstructions(t *testing.T) {
	for _, p := range []string{"code", "chat", "generic"} {
		got := ProfileInstructions(p, "know")
		if got == "" {
			t.Fatalf("ProfileInstructions(%q) returned empty string", p)
		}
	}

	code := ProfileInstructions("code", "know")
	unknown := ProfileInstructions("unknown", "know")
	if code != unknown {
		t.Fatal("unknown profile should fall back to code")
	}

	chat := ProfileInstructions("chat", "know")
	if code == chat {
		t.Fatal("code and chat profiles should differ")
	}
}

func TestProfileInstructionsCustomOntologyRoot(t *testing.T) {
	text := ProfileInstructions("code", "facts")
	if !strings.Contains(text, "facts/") {
		t.Errorf("ProfileInstructions should interpolate ontologyRoot, got: %s", text)
	}
	if strings.Contains(text, "know/") {
		t.Errorf("ProfileInstructions should not contain hardcoded 'know/', got: %s", text)
	}
}
