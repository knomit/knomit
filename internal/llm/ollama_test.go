package llm

import (
	"context"
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
