package llm

import "testing"

// TestResolveProvider pins the model→provider mapping. This is the boot-path
// decision (internal/app/app.go calls it to pick the adapter), and every rule
// here is a prefix match that is easy to break by reordering.
func TestResolveProvider(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		explicit string
		want     string
	}{
		// An explicit provider short-circuits every heuristic — including for
		// a model name that would otherwise infer something different.
		{"explicit wins over inference", "claude-sonnet-4-6", "ollama", "ollama"},
		{"explicit wins for unknown model", "whatever", "bedrock", "bedrock"},

		// Bare names are the CLI shorthand, and must be checked before the
		// "claude-"/"gemini-" prefix rules below.
		{"bare claude is cli shorthand", "claude", "", "claudecli"},
		{"bare gemini is cli shorthand", "gemini", "", "geminicli"},

		{"claude- prefix is anthropic api", "claude-sonnet-4-6", "", "anthropic"},
		{"gemini- prefix is gemini api", "gemini-2.0-flash", "", "gemini"},

		{"anthropic. prefix is bedrock", "anthropic.claude-3-5-sonnet", "", "bedrock"},
		{"us. prefix is bedrock", "us.anthropic.claude-3-5-sonnet", "", "bedrock"},
		{"eu. prefix is bedrock", "eu.anthropic.claude-3-5-sonnet", "", "bedrock"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveProvider(tt.model, tt.explicit)
			if err != nil {
				t.Fatalf("ResolveProvider(%q, %q) returned error: %v", tt.model, tt.explicit, err)
			}
			if got != tt.want {
				t.Errorf("ResolveProvider(%q, %q) = %q, want %q", tt.model, tt.explicit, got, tt.want)
			}
		})
	}
}

// TestResolveProvider_NoRuleMatches covers the error path: a model that matches
// no prefix and carries no explicit provider is a configuration error, not a
// silent fallback to some default adapter.
func TestResolveProvider_NoRuleMatches(t *testing.T) {
	for _, model := range []string{"unknown-model", "gpt-4", "", "claudecli"} {
		if got, err := ResolveProvider(model, ""); err == nil {
			t.Errorf("ResolveProvider(%q, \"\") = %q, want an error", model, got)
		}
	}
}

// TestResolveProvider_ExplicitIsNotValidated documents that ResolveProvider
// does NOT check the provider it is handed — validation lives in NewAdapter's
// default branch. A test asserting rejection here would be testing the wrong
// layer (the deleted TypeScript predecessor did exactly that).
func TestResolveProvider_ExplicitIsNotValidated(t *testing.T) {
	got, err := ResolveProvider("claude-sonnet-4-6", "not-a-real-provider")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "not-a-real-provider" {
		t.Errorf("got %q, want the explicit value passed through unchanged", got)
	}
}
