package synthesize

import (
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"
	"knomit/internal/git"
	"knomit/internal/store"
)

// TestReviewLoopIntegration exercises the full review loop end-to-end:
// StartSession → ContinueSession (prune) → ContinueSession (distill) → done,
// using a real git store and real SQLite-backed ReviewIndex.
func TestReviewLoopIntegration(t *testing.T) {
	ctrl := gomock.NewController(t)

	// Real SQLite store for ReviewIndex.
	dir := t.TempDir()
	svc, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	reviewIdx := svc.Index()

	// Real git store backed by SQLite storer.
	gitStore, err := git.InitWithStorer(svc.GitStorer(), nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Write 3 facts to real git so gatherAllFacts and ReadFile work.
	facts := map[string]string{
		"kb/go/concurrency.md": factContent("Go Concurrency", "Go uses goroutines and channels for concurrency."),
		"kb/go/interfaces.md":  factContent("Go Interfaces", "Go interfaces are satisfied implicitly."),
		"kb/go/errors.md":      factContent("Go Error Handling", "Go uses explicit error returns instead of exceptions."),
	}
	for path, content := range facts {
		if _, _, err := gitStore.WriteFile(path, content, "add "+path, "learn"); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// Mock SearchIndex.
	idx := NewMockSearchIndex(ctrl)
	// dirtyFacts (no watermark) calls Search with Limit=100_000 to get all facts from index.
	idx.EXPECT().Search(store.SearchQuery{Limit: 100_000}).Return([]store.SearchResult{
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/concurrency.md", Title: "Go Concurrency", Type: "observation", Domain: []string{"testing"}, Confidence: 0.8, Sources: 1}, Body: "Go uses goroutines and channels for concurrency."}},
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/interfaces.md", Title: "Go Interfaces", Type: "observation", Domain: []string{"testing"}, Confidence: 0.8, Sources: 1}, Body: "Go interfaces are satisfied implicitly."}},
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/errors.md", Title: "Go Error Handling", Type: "observation", Domain: []string{"testing"}, Confidence: 0.8, Sources: 1}, Body: "Go uses explicit error returns instead of exceptions."}},
	}, nil)
	// ScopedCluster calls Search for neighbors.
	idx.EXPECT().Search(gomock.Any()).Return(nil, nil).AnyTimes()
	idx.EXPECT().ClusterFacts(gomock.Any(), gomock.Any()).Return(store.ClusterResult{}, fmt.Errorf("no embeddings")).AnyTimes()
	// For prune/distill apply: delete calls go to real git, index delete goes to mock.
	idx.EXPECT().Delete(gomock.Any()).Return(nil).AnyTimes()
	idx.EXPECT().Upsert(gomock.Any()).Return(nil).AnyTimes()
	idx.EXPECT().GraphAddDerivedFrom(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	r := NewReviewer(gitStore, idx, reviewIdx, NewMockEmbedder(ctrl), nil)

	// --- Step 1: StartSession — all 3 facts are dirty (no watermark). ---
	result, err := r.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if result.Done {
		t.Fatal("expected work items, got Done=true")
	}
	if result.Item == nil {
		t.Fatal("expected non-nil Item from StartSession")
	}
	sessionID := result.SessionID
	if sessionID == "" {
		t.Fatal("expected non-empty session ID")
	}

	// Should start with a prune item (prune items have higher priority).
	if result.Item.Type != "prune" {
		t.Fatalf("expected first item type=prune, got %s", result.Item.Type)
	}
	t.Logf("StartSession: session=%s, type=%s, progress=%d/%d",
		sessionID, result.Item.Type, result.Progress.Completed, result.Progress.Remaining)

	// --- Step 2: Answer the prune item — retract errors.md, keep the rest. ---
	pruneResp := `{"decisions": [
		{"path": "kb/go/concurrency.md", "action": "keep"},
		{"path": "kb/go/interfaces.md", "action": "keep"},
		{"path": "kb/go/errors.md", "action": "retract"}
	]}`

	result, err = r.ContinueSession(sessionID, pruneResp)
	if err != nil {
		t.Fatalf("ContinueSession (prune): %v", err)
	}
	t.Logf("After prune: done=%v, item=%v, progress=%+v",
		result.Done, result.Item != nil, result.Progress)

	// Verify retracted fact is actually deleted from git.
	_, readErr := gitStore.ReadFile("kb/go/errors.md")
	if readErr == nil {
		t.Error("expected kb/go/errors.md to be deleted from git after retract")
	}

	// Kept facts should still be readable.
	if _, err := gitStore.ReadFile("kb/go/concurrency.md"); err != nil {
		t.Errorf("concurrency.md should still exist: %v", err)
	}
	if _, err := gitStore.ReadFile("kb/go/interfaces.md"); err != nil {
		t.Errorf("interfaces.md should still exist: %v", err)
	}

	// --- Step 3: If there's a distill item, answer it. ---
	for !result.Done {
		if result.Item == nil {
			t.Fatal("not done but no item returned")
		}
		t.Logf("Processing: type=%s, progress=%d/%d",
			result.Item.Type, result.Progress.Completed, result.Progress.Remaining)

		var resp string
		switch result.Item.Type {
		case "distill":
			// Synthesize a combined fact, retract one of the inputs.
			resp = `{"synthesize": [{"path": "kb/go/combined.md", "title": "Go Combined", "body": "Go is great.", "type": "observation", "domain": ["go"], "confidence": 0.9, "entities": [], "refs": ["kb/go/concurrency.md", "kb/go/interfaces.md"]}], "retract": ["kb/go/concurrency.md"]}`
		case "prune":
			// Keep everything in any additional prune clusters.
			resp = `{"decisions": [{"path": "kb/go/concurrency.md", "action": "keep"}, {"path": "kb/go/interfaces.md", "action": "keep"}, {"path": "kb/go/errors.md", "action": "keep"}]}`
		default:
			t.Fatalf("unexpected item type: %s", result.Item.Type)
		}

		result, err = r.ContinueSession(sessionID, resp)
		if err != nil {
			t.Fatalf("ContinueSession (%s): %v", result.Item, err)
		}
	}

	// --- Step 4: Session should now be done. ---
	if !result.Done {
		t.Error("expected Done=true after processing all items")
	}
	if result.Progress == nil {
		t.Fatal("expected non-nil Progress in final result")
	}
	t.Logf("Session complete: completed=%d", result.Progress.Completed)

	// --- Step 5: Verify watermark was advanced. ---
	watermark, err := reviewIdx.GetReviewWatermark(gitStore.Branch())
	if err != nil {
		t.Fatalf("GetReviewWatermark: %v", err)
	}
	if watermark == "" {
		t.Fatal("expected watermark to be set after session completes")
	}
	headHash, err := gitStore.HeadCommit()
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	t.Logf("Watermark=%s, HEAD=%s", watermark, headHash)

	// --- Step 6: StartSession again — should return done (nothing changed). ---
	result2, err := r.StartSession()
	if err != nil {
		t.Fatalf("second StartSession: %v", err)
	}
	if !result2.Done {
		t.Errorf("expected second StartSession to return Done=true (no changes since watermark), got Done=false with item type=%v", result2.Item)
	}
	t.Logf("Second StartSession: done=%v", result2.Done)

	// --- Step 7: Verify session was persisted and completed. ---
	sess, err := reviewIdx.GetReviewSession(sessionID)
	if err != nil {
		t.Fatalf("GetReviewSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session to still be retrievable")
	}
	if sess.Status != "completed" {
		t.Errorf("expected session status=completed, got %s", sess.Status)
	}
}
