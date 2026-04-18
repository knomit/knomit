package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/store"
)

func TestWriteStoreError_BranchNotFound_Returns404(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/repos/r/branches/x/topics", nil)
	err := fmt.Errorf("ListDir: ref: %w", store.ErrBranchNotFound)

	writeStoreError(w, r, err, "Failed to list topics", "x")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	var body map[string]any
	if decErr := json.Unmarshal(w.Body.Bytes(), &body); decErr != nil {
		t.Fatalf("decode body: %v", decErr)
	}
	if body["title"] != "Branch not found" {
		t.Errorf("title = %q, want Branch not found", body["title"])
	}
}

func TestWriteStoreError_GenericError_Returns500(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/repos/r/branches/x/topics", nil)
	err := errors.New("boom")

	writeStoreError(w, r, err, "Failed to list topics", "x")

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	var body map[string]any
	if decErr := json.Unmarshal(w.Body.Bytes(), &body); decErr != nil {
		t.Fatalf("decode body: %v", decErr)
	}
	if body["title"] != "Failed to list topics" {
		t.Errorf("title = %q, want Failed to list topics", body["title"])
	}
}
