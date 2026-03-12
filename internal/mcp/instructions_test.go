package mcp

import "testing"

func TestProfileInstructions(t *testing.T) {
	for _, p := range []string{"code", "chat", "generic"} {
		got := ProfileInstructions(p)
		if got == "" {
			t.Fatalf("ProfileInstructions(%q) returned empty string", p)
		}
	}

	code := ProfileInstructions("code")
	unknown := ProfileInstructions("unknown")
	if code != unknown {
		t.Fatal("unknown profile should fall back to code")
	}

	chat := ProfileInstructions("chat")
	if code == chat {
		t.Fatal("code and chat profiles should differ")
	}
}
