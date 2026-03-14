package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

func TestAnthropicAdapter_Complete(t *testing.T) {
	// SSE response simulating two text chunks followed by message_stop.
	sseBody := "" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sseBody)
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(
		"claude-3-5-haiku-20241022",
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test-key"),
	)

	var chunks []string
	result, err := adapter.Complete(context.Background(), "system prompt", []Message{
		{Role: "user", Content: "say hello"},
	}, CompletionOptions{}, func(chunk string) {
		chunks = append(chunks, chunk)
	})

	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	if result != "hello world" {
		t.Errorf("Complete() = %q, want %q", result, "hello world")
	}

	if len(chunks) != 2 {
		t.Fatalf("onChunk called %d times, want 2", len(chunks))
	}
	if chunks[0] != "hello" {
		t.Errorf("chunks[0] = %q, want %q", chunks[0], "hello")
	}
	if chunks[1] != " world" {
		t.Errorf("chunks[1] = %q, want %q", chunks[1], " world")
	}
}

func TestNewAdapter_Ollama_NoServer(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:19999")
	_, err := NewAdapter(context.Background(), "ollama", "qwen3:8b")
	if err == nil {
		t.Fatal("expected error when Ollama is unreachable, got nil")
	}
	if strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("resolver should recognize ollama, got: %v", err)
	}
}

func TestResolveProvider(t *testing.T) {
	tests := []struct {
		model    string
		explicit string
		want     string
		wantErr  bool
	}{
		{"claude-3-5-sonnet", "", "anthropic", false},
		{"gemini-1.5-pro", "", "gemini", false},
		{"anthropic.claude-3", "", "bedrock", false},
		{"us.anthropic.claude-3", "", "bedrock", false},
		{"eu.anthropic.claude-3", "", "bedrock", false},
		{"unknown-model", "", "", true},
		{"unknown-model", "anthropic", "anthropic", false},
		{"claude", "", "claudecli", false},
		{"gemini", "", "geminicli", false},
	}

	for _, tc := range tests {
		got, err := ResolveProvider(tc.model, tc.explicit)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ResolveProvider(%q, %q): expected error", tc.model, tc.explicit)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveProvider(%q, %q) unexpected error: %v", tc.model, tc.explicit, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ResolveProvider(%q, %q) = %q, want %q", tc.model, tc.explicit, got, tc.want)
		}
	}
}
