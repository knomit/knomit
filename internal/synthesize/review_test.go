package synthesize

import (
	"encoding/json"
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"
	"knomit/internal/store"
)

func TestStartSession_NoDirtyFacts(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	gs.EXPECT().Branch().Return("machine/test")
	ri.EXPECT().GCReviewSessions("machine/test", 5).Return(nil)
	ri.EXPECT().CreateReviewSession("machine/test").Return(&store.ReviewSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	// No watermark → all facts dirty, but index returns empty.
	ri.EXPECT().GetReviewWatermark("machine/test").Return("", nil)
	idx.EXPECT().Search(store.SearchQuery{Limit: 100_000}).Return(nil, nil)

	// Complete session immediately.
	ri.EXPECT().CompleteReviewSession("sess-1").Return(nil)
	gs.EXPECT().HeadCommit().Return("abc123", nil)
	ri.EXPECT().SetReviewWatermark("machine/test", "abc123").Return(nil)
	ri.EXPECT().WorkItemStats("sess-1").Return(0, 0, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.StartSession()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Error("expected Done=true for no dirty facts")
	}
	if result.SessionID != "sess-1" {
		t.Errorf("expected session ID sess-1, got %s", result.SessionID)
	}
}

// TestStartSession_WatermarkAtHead_NoDirtyFacts verifies that when the review
// watermark is set to HEAD (e.g. after fresh InitFromRemote), DiffFiles returns
// no changes and the review completes immediately without processing any facts.
func TestStartSession_WatermarkAtHead_NoDirtyFacts(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	gs.EXPECT().Branch().Return("machine/test")
	ri.EXPECT().GCReviewSessions("machine/test", 5).Return(nil)
	ri.EXPECT().CreateReviewSession("machine/test").Return(&store.ReviewSession{
		ID: "sess-wm", Branch: "machine/test", Status: "active",
	}, nil)

	// Watermark is set to HEAD — simulates post-InitFromRemote state.
	ri.EXPECT().GetReviewWatermark("machine/test").Return("head-hash", nil)

	// DiffFiles returns no changes since watermark == HEAD.
	gs.EXPECT().DiffFiles("head-hash").Return(nil, nil, nil, nil)

	// No dirty facts → session completes immediately.
	ri.EXPECT().CompleteReviewSession("sess-wm").Return(nil)
	gs.EXPECT().HeadCommit().Return("head-hash", nil)
	ri.EXPECT().SetReviewWatermark("machine/test", "head-hash").Return(nil)
	ri.EXPECT().WorkItemStats("sess-wm").Return(0, 0, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.StartSession()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Error("expected Done=true when watermark is at HEAD")
	}
}

func TestStartSession_WatermarkEmpty_AllFactsDirty(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	gs.EXPECT().Branch().Return("machine/test")
	ri.EXPECT().GCReviewSessions("machine/test", 5).Return(nil)
	ri.EXPECT().CreateReviewSession("machine/test").Return(&store.ReviewSession{
		ID: "sess-2", Branch: "machine/test", Status: "active",
	}, nil)

	ri.EXPECT().GetReviewWatermark("machine/test").Return("", nil)

	// No watermark → index returns all facts.
	idx.EXPECT().Search(store.SearchQuery{Limit: 100_000}).Return([]store.SearchResult{
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/one.md", Title: "Fact one", Type: "observation", Domain: []string{"testing"}, Confidence: 0.8, Sources: 1}, Body: "Body one."}},
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/two.md", Title: "Fact two", Type: "observation", Domain: []string{"testing"}, Confidence: 0.8, Sources: 1}, Body: "Body two."}},
	}, nil)

	// ScopedCluster will search for neighbors.
	idx.EXPECT().Search(gomock.Any()).Return(nil, nil).AnyTimes()
	// ClusterFacts fails → fallback to category grouping (both in kb/go → one cluster).
	idx.EXPECT().ClusterFacts(1.0, 2).Return(store.ClusterResult{}, fmt.Errorf("no embeddings"))

	// Should insert one prune work item (cluster of 2) and one distill item (>1 seed).
	ri.EXPECT().InsertWorkItem(gomock.Any()).DoAndReturn(func(item store.ReviewWorkItem) error {
		if item.StepType != "prune" && item.StepType != "distill" {
			t.Errorf("unexpected step type: %s", item.StepType)
		}
		return nil
	}).Times(2)

	// nextItem call.
	ri.EXPECT().NextWorkItem("sess-2").Return(&store.ReviewWorkItem{
		ID:        1,
		SessionID: "sess-2",
		StepType:  "prune",
		FactsJSON: mustJSON(t, []factForLLM{
			{File: "kb/go/one.md", Title: "Fact one", Body: "Body one."},
			{File: "kb/go/two.md", Title: "Fact two", Body: "Body two."},
		}),
		Priority: 2,
	}, nil)
	ri.EXPECT().WorkItemStats("sess-2").Return(0, 2, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.StartSession()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Done {
		t.Error("expected Done=false, got true")
	}
	if result.Item == nil {
		t.Fatal("expected non-nil Item")
	}
	if result.Item.Type != "prune" {
		t.Errorf("expected prune item, got %s", result.Item.Type)
	}
	if result.Progress == nil {
		t.Fatal("expected non-nil Progress")
	}
	if result.Progress.Remaining != 2 {
		t.Errorf("expected 2 remaining, got %d", result.Progress.Remaining)
	}
}

func TestStartSession_WithWatermark(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	gs.EXPECT().Branch().Return("machine/test")
	ri.EXPECT().GCReviewSessions("machine/test", 5).Return(nil)
	ri.EXPECT().CreateReviewSession("machine/test").Return(&store.ReviewSession{
		ID: "sess-3", Branch: "machine/test", Status: "active",
	}, nil)

	ri.EXPECT().GetReviewWatermark("machine/test").Return("old-hash", nil)

	// DiffFiles: only one.md was modified since watermark.
	gs.EXPECT().DiffFiles("old-hash").Return(nil, []string{"kb/go/one.md"}, nil, nil)

	// Incremental: only reads the changed file.
	fact1Content := factContent("Fact one", "Body one.")
	gs.EXPECT().ReadFile("kb/go/one.md").Return(fact1Content, nil)

	// ScopedCluster: one seed (one.md), searches neighbors.
	// Dedup also calls Search with MinSimilarity=0.92; return empty for those.
	idx.EXPECT().Search(gomock.Any()).DoAndReturn(func(q store.SearchQuery) ([]store.SearchResult, error) {
		if q.MinSimilarity >= 0.9 {
			return nil, nil // dedup pass: no near-duplicates
		}
		return []store.SearchResult{
			{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/two.md"}}},
		}, nil
	}).AnyTimes()
	idx.EXPECT().ClusterFacts(1.0, 2).Return(store.ClusterResult{
		Clusters: map[int][]string{0: {"kb/go/one.md", "kb/go/two.md"}},
	}, nil)

	// One prune item inserted. No distill (only 1 seed).
	ri.EXPECT().InsertWorkItem(gomock.Any()).DoAndReturn(func(item store.ReviewWorkItem) error {
		if item.StepType != "prune" {
			t.Errorf("expected prune, got %s", item.StepType)
		}
		return nil
	}).Times(1)

	// nextItem call.
	ri.EXPECT().NextWorkItem("sess-3").Return(&store.ReviewWorkItem{
		ID:        1,
		SessionID: "sess-3",
		StepType:  "prune",
		FactsJSON: mustJSON(t, []factForLLM{
			{File: "kb/go/one.md", Title: "Fact one", Body: "Body one."},
			{File: "kb/go/two.md", Title: "Fact two", Body: "Body two."},
		}),
		Priority: 2,
	}, nil)
	ri.EXPECT().WorkItemStats("sess-3").Return(0, 1, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.StartSession()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Done {
		t.Error("expected Done=false")
	}
	if result.Item == nil {
		t.Fatal("expected item")
	}
}

func TestContinueSession_PruneResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	ri.EXPECT().GetReviewSession("sess-1").Return(&store.ReviewSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	facts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
		{File: "kb/go/two.md", Title: "Fact two", Body: "Body two.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}
	ri.EXPECT().NextWorkItem("sess-1").Return(&store.ReviewWorkItem{
		ID:        1,
		SessionID: "sess-1",
		StepType:  "prune",
		FactsJSON: mustJSON(t, facts),
		Priority:  2,
	}, nil)

	// The prune response keeps one and retracts one.
	pruneResp := `{"decisions": [{"path": "kb/go/one.md", "action": "keep"}, {"path": "kb/go/two.md", "action": "retract"}]}`

	// ApplyPruneDecisions: retract two.md.
	gs.EXPECT().DeleteFile("kb/go/two.md", gomock.Any(), gomock.Any()).Return("c1", nil)
	idx.EXPECT().Delete("kb/go/two.md").Return(nil)


	ri.EXPECT().SetWorkItemResponse(int64(1), pruneResp).Return(nil)

	// nextItem: no more items → complete session.
	ri.EXPECT().NextWorkItem("sess-1").Return(nil, nil)
	ri.EXPECT().GetReviewSession("sess-1").Return(&store.ReviewSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)
	ri.EXPECT().CompleteReviewSession("sess-1").Return(nil)
	gs.EXPECT().HeadCommit().Return("new-hash", nil)
	ri.EXPECT().SetReviewWatermark("machine/test", "new-hash").Return(nil)
	ri.EXPECT().WorkItemStats("sess-1").Return(1, 0, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.ContinueSession("sess-1", pruneResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Error("expected Done=true after last item")
	}
	if result.Progress.Completed != 1 {
		t.Errorf("expected 1 completed, got %d", result.Progress.Completed)
	}
}

func TestContinueSession_ReturnsNextItem(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	ri.EXPECT().GetReviewSession("sess-1").Return(&store.ReviewSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	facts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}
	ri.EXPECT().NextWorkItem("sess-1").Return(&store.ReviewWorkItem{
		ID:        1,
		SessionID: "sess-1",
		StepType:  "prune",
		FactsJSON: mustJSON(t, facts),
		Priority:  2,
	}, nil)

	// Keep everything — no side effects from ApplyPruneDecisions.
	pruneResp := `{"decisions": [{"path": "kb/go/one.md", "action": "keep"}]}`
	ri.EXPECT().SetWorkItemResponse(int64(1), pruneResp).Return(nil)

	// nextItem: another item waiting.
	nextFacts := []factForLLM{
		{File: "kb/go/three.md", Title: "Fact three", Body: "Body three.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
		{File: "kb/go/four.md", Title: "Fact four", Body: "Body four.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}
	ri.EXPECT().NextWorkItem("sess-1").Return(&store.ReviewWorkItem{
		ID:        2,
		SessionID: "sess-1",
		StepType:  "distill",
		FactsJSON: mustJSON(t, nextFacts),
		Priority:  0,
	}, nil)
	ri.EXPECT().WorkItemStats("sess-1").Return(1, 1, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.ContinueSession("sess-1", pruneResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Done {
		t.Error("expected Done=false, more items remain")
	}
	if result.Item == nil {
		t.Fatal("expected non-nil Item")
	}
	if result.Item.Type != "distill" {
		t.Errorf("expected distill item, got %s", result.Item.Type)
	}
}

func TestContinueSession_InvalidSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	ri.EXPECT().GetReviewSession("bad-id").Return(nil, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	_, err := r.ContinueSession("bad-id", "{}")
	if err == nil {
		t.Fatal("expected error for invalid session")
	}
}

func TestContinueSession_CompletedSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	ri.EXPECT().GetReviewSession("done-sess").Return(&store.ReviewSession{
		ID: "done-sess", Branch: "machine/test", Status: "completed",
	}, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	_, err := r.ContinueSession("done-sess", "{}")
	if err == nil {
		t.Fatal("expected error for completed session")
	}
}

func TestContinueSession_InvalidJSON(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	ri.EXPECT().GetReviewSession("sess-1").Return(&store.ReviewSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	facts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one."},
	}
	ri.EXPECT().NextWorkItem("sess-1").Return(&store.ReviewWorkItem{
		ID:        1,
		SessionID: "sess-1",
		StepType:  "prune",
		FactsJSON: mustJSON(t, facts),
		Priority:  2,
	}, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	_, err := r.ContinueSession("sess-1", "not valid json at all")
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestContinueSession_DistillResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	ri.EXPECT().GetReviewSession("sess-1").Return(&store.ReviewSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	facts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
		{File: "kb/go/two.md", Title: "Fact two", Body: "Body two.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}
	ri.EXPECT().NextWorkItem("sess-1").Return(&store.ReviewWorkItem{
		ID:        2,
		SessionID: "sess-1",
		StepType:  "distill",
		FactsJSON: mustJSON(t, facts),
		Priority:  0,
	}, nil)

	distillResp := `{"synthesize": [{"path": "kb/go/combined.md", "title": "Combined", "body": "Merged insight.", "type": "observation", "domain": ["go"], "confidence": 0.9, "entities": [], "refs": ["kb/go/one.md", "kb/go/two.md"]}], "retract": ["kb/go/one.md"]}`

	// ApplyDistillDecisions: write synth (path gets UUID), retract one.md.
	gs.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("c1", "b1", nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)
	idx.EXPECT().GraphAddDerivedFrom(gomock.Any(), gomock.Any()).Return(nil)

	gs.EXPECT().DeleteFile("kb/go/one.md", gomock.Any(), gomock.Any()).Return("c2", nil)
	idx.EXPECT().Delete("kb/go/one.md").Return(nil)

	ri.EXPECT().SetWorkItemResponse(int64(2), distillResp).Return(nil)

	// nextItem: done.
	ri.EXPECT().NextWorkItem("sess-1").Return(nil, nil)
	ri.EXPECT().GetReviewSession("sess-1").Return(&store.ReviewSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)
	ri.EXPECT().CompleteReviewSession("sess-1").Return(nil)
	gs.EXPECT().HeadCommit().Return("new-hash", nil)
	ri.EXPECT().SetReviewWatermark("machine/test", "new-hash").Return(nil)
	ri.EXPECT().WorkItemStats("sess-1").Return(1, 0, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.ContinueSession("sess-1", distillResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Error("expected Done=true")
	}
}

func TestContinueSession_ValidationError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	ri.EXPECT().GetReviewSession("sess-1").Return(&store.ReviewSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	facts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one."},
	}
	ri.EXPECT().NextWorkItem("sess-1").Return(&store.ReviewWorkItem{
		ID:        1,
		SessionID: "sess-1",
		StepType:  "prune",
		FactsJSON: mustJSON(t, facts),
		Priority:  2,
	}, nil)

	// Response references unknown path.
	badResp := `{"decisions": [{"path": "kb/go/nonexistent.md", "action": "retract"}]}`

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	_, err := r.ContinueSession("sess-1", badResp)
	if err == nil {
		t.Fatal("expected error for validation failure")
	}
}

func TestDirtyFacts_NoWatermark_UsesIndex(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	ri.EXPECT().GetReviewWatermark("machine/test").Return("", nil)
	idx.EXPECT().Search(store.SearchQuery{Limit: 100_000}).Return([]store.SearchResult{
		{FactWithBody: store.FactWithBody{
			FactRecord: store.FactRecord{Path: "kb/go/one.md", Title: "Fact one", Type: "observation", Domain: []string{"go"}, Entities: []string{"Go"}, Confidence: 0.9, Sources: 2},
			Body:       "Body one.",
		}},
		{FactWithBody: store.FactWithBody{
			FactRecord: store.FactRecord{Path: "kb/go/two.md", Title: "Fact two", Type: "insight", Domain: []string{"go"}, Confidence: 0.7, Sources: 1},
			Body:       "Body two.",
		}},
	}, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	facts, err := r.dirtyFacts("machine/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}
	if facts[0].File != "kb/go/one.md" {
		t.Errorf("expected path kb/go/one.md, got %s", facts[0].File)
	}
	if facts[0].Title != "Fact one" {
		t.Errorf("expected title 'Fact one', got %s", facts[0].Title)
	}
	if facts[1].Type != "insight" {
		t.Errorf("expected type 'insight', got %s", facts[1].Type)
	}
}

func TestDirtyFacts_Incremental_OnlyChangedFiles(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	ri.EXPECT().GetReviewWatermark("machine/test").Return("abc123", nil)
	gs.EXPECT().DiffFiles("abc123").Return(
		[]string{"kb/go/new.md"},           // added
		[]string{"kb/go/changed.md"},       // modified
		[]string{"kb/go/deleted.md"},       // deleted (not read)
		nil,
	)

	newContent := factContent("New fact", "Brand new.")
	changedContent := factContent("Changed fact", "Updated body.")
	gs.EXPECT().ReadFile("kb/go/new.md").Return(newContent, nil)
	gs.EXPECT().ReadFile("kb/go/changed.md").Return(changedContent, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	facts, err := r.dirtyFacts("machine/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}
	titles := map[string]bool{facts[0].Title: true, facts[1].Title: true}
	if !titles["New fact"] || !titles["Changed fact"] {
		t.Errorf("expected 'New fact' and 'Changed fact', got %v", titles)
	}
}

func TestDirtyFacts_Incremental_SkipsDeletedAndNonMD(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockReviewIndex(ctrl)

	ri.EXPECT().GetReviewWatermark("machine/test").Return("abc123", nil)
	gs.EXPECT().DiffFiles("abc123").Return(
		[]string{"kb/go/gone.md", "README.txt"}, // added: one .md (unreadable), one non-.md
		nil, nil, nil,
	)

	// gone.md returns error (deleted between diff and read).
	gs.EXPECT().ReadFile("kb/go/gone.md").Return("", fmt.Errorf("not found"))
	// README.txt should not be read at all (not .md).

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	facts, err := r.dirtyFacts("machine/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("expected 0 facts, got %d", len(facts))
	}
}

// mustJSON marshals v to JSON string, failing the test on error.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustJSON: %v", err)
	}
	return string(b)
}
