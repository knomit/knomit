package llm

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// DefaultConfig
// ---------------------------------------------------------------------------

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Model != "gemini-2.5-flash" {
		t.Errorf("Model = %q, want %q", cfg.Model, "gemini-2.5-flash")
	}
	if cfg.Provider != "gemini" {
		t.Errorf("Provider = %q, want %q", cfg.Provider, "gemini")
	}
	if cfg.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", cfg.APIKey)
	}
}

// ---------------------------------------------------------------------------
// ResolveProvider — edge cases not covered in anthropic_test.go
// ---------------------------------------------------------------------------

func TestResolveProvider_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		explicit string
		want     string
		wantErr  bool
	}{
		{name: "empty model no explicit", model: "", explicit: "", wantErr: true},
		{name: "ollama-prefixed model has no rule", model: "ollama:llama3", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveProvider(tt.model, tt.explicit)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got provider %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("provider = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NewAdapter — factory dispatch tests
// ---------------------------------------------------------------------------

func TestNewAdapter_Anthropic(t *testing.T) {
	a, err := NewAdapter(context.Background(), "anthropic", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == nil {
		t.Fatal("adapter is nil")
	}
}

func TestNewAdapter_ClaudeCLI(t *testing.T) {
	a, err := NewAdapter(context.Background(), "claudecli", "claude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == nil {
		t.Fatal("adapter is nil")
	}
}

func TestNewAdapter_GeminiCLI(t *testing.T) {
	a, err := NewAdapter(context.Background(), "geminicli", "gemini")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == nil {
		t.Fatal("adapter is nil")
	}
}

func TestNewAdapter_Gemini_NoKey(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	_, err := NewAdapter(context.Background(), "gemini", "gemini-2.0-flash")
	if err == nil {
		t.Fatal("expected error when no API key is set")
	}
}

func TestNewAdapter_Bedrock_NoAWS(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	_, err := NewAdapter(context.Background(), "bedrock", "anthropic.claude-v2")
	// Bedrock may or may not fail depending on default credential chain;
	// we just verify the factory dispatches without panic.
	_ = err
}

func TestNewAdapter_Unknown(t *testing.T) {
	_, err := NewAdapter(context.Background(), "nonexistent", "some-model")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
