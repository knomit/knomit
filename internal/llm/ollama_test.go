package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOllamaAdapter_New_HealthCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" && r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[]}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_HOST", srv.URL)
	adapter, err := NewOllamaAdapter(context.Background(), "qwen3:8b")
	if err != nil {
		t.Fatalf("NewOllamaAdapter() error: %v", err)
	}
	if adapter == nil {
		t.Fatal("adapter is nil")
	}
}

func TestOllamaAdapter_New_Unreachable(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:19999")
	_, err := NewOllamaAdapter(context.Background(), "qwen3:8b")
	if err == nil {
		t.Fatal("expected error for unreachable Ollama")
	}
}

func TestOllamaAdapter_Complete(t *testing.T) {
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[]}`))
			return
		}
		if r.URL.Path == "/api/chat" && r.Method == http.MethodPost {
			json.NewDecoder(r.Body).Decode(&capturedBody)

			lines := []string{
				`{"message":{"role":"assistant","content":"hello"},"done":false}`,
				`{"message":{"role":"assistant","content":" world"},"done":false}`,
				`{"message":{"role":"assistant","content":""},"done":true}`,
			}
			for _, line := range lines {
				fmt.Fprintln(w, line)
			}
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_HOST", srv.URL)
	adapter, err := NewOllamaAdapter(context.Background(), "qwen3:8b")
	if err != nil {
		t.Fatalf("NewOllamaAdapter: %v", err)
	}

	var chunks []string
	result, err := adapter.Complete(context.Background(), "you are helpful", []Message{
		{Role: "user", Content: "say hello"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "now"},
	}, func(chunk string) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	if result != "hello world" {
		t.Errorf("result = %q, want %q", result, "hello world")
	}

	if len(chunks) != 2 {
		t.Fatalf("onChunk called %d times, want 2", len(chunks))
	}
	if chunks[0] != "hello" || chunks[1] != " world" {
		t.Errorf("chunks = %v, want [hello, ' world']", chunks)
	}

	if capturedBody["model"] != "qwen3:8b" {
		t.Errorf("model = %v, want qwen3:8b", capturedBody["model"])
	}
	if capturedBody["format"] != "json" {
		t.Errorf("format = %v, want json", capturedBody["format"])
	}
	if capturedBody["stream"] != true {
		t.Errorf("stream = %v, want true", capturedBody["stream"])
	}

	msgs, ok := capturedBody["messages"].([]any)
	if !ok {
		t.Fatalf("messages not an array: %T", capturedBody["messages"])
	}
	if len(msgs) != 4 {
		t.Fatalf("len(messages) = %d, want 4 (system + 3)", len(msgs))
	}
	firstMsg := msgs[0].(map[string]any)
	if firstMsg["role"] != "system" {
		t.Errorf("messages[0].role = %v, want system", firstMsg["role"])
	}
}
