package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/engine/store/model"
)

func TestWriteStoreError_BranchNotFound_Returns404(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/repos/r/branches/x/topics", nil)
	err := fmt.Errorf("ListDir: ref: %w", model.ErrBranchNotFound)

	WriteStoreError(w, r, err, "Failed to list topics", "x")

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

// A commit the repo does not hold, wrapped anywhere in the chain, is 404
// with code commit_not_in_repo: never the 500 fallthrough.
func TestWriteStoreError_CommitNotFound_Returns404WithCode(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/repos/r/branches/x/commits", nil)
	err := fmt.Errorf("LogPaginated: CommitLogQuery: after lookup: %w", model.ErrCommitNotFound)

	WriteStoreError(w, r, err, "Failed to list commits", "x")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	var body map[string]any
	if decErr := json.Unmarshal(w.Body.Bytes(), &body); decErr != nil {
		t.Fatalf("decode body: %v", decErr)
	}
	if body["title"] != "Commit not found" || body["code"] != CodeCommitNotInRepo {
		t.Errorf("body = %v, want title Commit not found and code %s", body, CodeCommitNotInRepo)
	}
}

func TestWriteStoreError_GenericError_Returns500(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/repos/r/branches/x/topics", nil)
	err := errors.New("boom")

	WriteStoreError(w, r, err, "Failed to list topics", "x")

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
