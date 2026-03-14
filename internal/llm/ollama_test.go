package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	}, CompletionOptions{}, func(chunk string) {
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
	if capturedBody["format"] != "" {
		t.Errorf("format = %v, want \"\" (ForceJSON=false)", capturedBody["format"])
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

func TestOllamaAdapter_Complete_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[]}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_HOST", srv.URL)
	adapter, _ := NewOllamaAdapter(context.Background(), "bad-model")

	_, err := adapter.Complete(context.Background(), "sys", []Message{
		{Role: "user", Content: "hi"},
	}, CompletionOptions{}, nil)
	if err == nil {
		t.Fatal("expected error for HTTP 400")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention status code: %v", err)
	}
}

func TestOllamaAdapter_Complete_NilOnChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[]}`))
			return
		}
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"ok"},"done":true}`)
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_HOST", srv.URL)
	adapter, _ := NewOllamaAdapter(context.Background(), "qwen3:8b")

	result, err := adapter.Complete(context.Background(), "sys", []Message{
		{Role: "user", Content: "hi"},
	}, CompletionOptions{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Errorf("result = %q, want %q", result, "ok")
	}
}

func TestOllamaAdapter_Complete_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[]}`))
			return
		}
		lines := []string{
			`{"message":{"role":"assistant","content":"one"},"done":false}`,
			`NOT VALID JSON`,
			`{"message":{"role":"assistant","content":" two"},"done":false}`,
			`{"message":{"role":"assistant","content":""},"done":true}`,
		}
		for _, line := range lines {
			fmt.Fprintln(w, line)
		}
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_HOST", srv.URL)
	adapter, _ := NewOllamaAdapter(context.Background(), "qwen3:8b")

	result, err := adapter.Complete(context.Background(), "sys", []Message{
		{Role: "user", Content: "hi"},
	}, CompletionOptions{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "one two" {
		t.Errorf("result = %q, want %q (malformed line should be skipped)", result, "one two")
	}
}

func TestOllamaAdapter_Complete_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[]}`))
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_HOST", srv.URL)
	adapter, _ := NewOllamaAdapter(context.Background(), "qwen3:8b")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := adapter.Complete(ctx, "sys", []Message{
		{Role: "user", Content: "hi"},
	}, CompletionOptions{}, nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}
