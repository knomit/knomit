package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"
	"knomit/internal/llm"
	"knomit/internal/store"
)

func TestStartSession_NoDirtyFacts(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockPipelineIndex(ctrl)

	gs.EXPECT().Branch().Return("machine/test")
	ri.EXPECT().GCPipelineSessions("review", "machine/test", 5).Return(nil)
	ri.EXPECT().CreatePipelineSession("review", "machine/test").Return(&store.PipelineSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	// No watermark → all facts dirty, but index returns empty.
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("", nil)
	idx.EXPECT().Search(store.SearchQuery{Limit: 100_000}).Return(nil, nil)

	// Complete session immediately.
	ri.EXPECT().CompletePipelineSession("sess-1").Return(nil)
	gs.EXPECT().HeadCommit().Return("abc123", nil)
	ri.EXPECT().SetPipelineWatermark("review", "machine/test", "abc123").Return(nil)
	ri.EXPECT().PipelineWorkItemStats("sess-1").Return(0, 0, nil)

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
	ri := NewMockPipelineIndex(ctrl)

	gs.EXPECT().Branch().Return("machine/test")
	ri.EXPECT().GCPipelineSessions("review", "machine/test", 5).Return(nil)
	ri.EXPECT().CreatePipelineSession("review", "machine/test").Return(&store.PipelineSession{
		ID: "sess-wm", Branch: "machine/test", Status: "active",
	}, nil)

	// Watermark is set to HEAD — simulates post-InitFromRemote state.
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("head-hash", nil)

	// DiffFiles returns no changes since watermark == HEAD.
	gs.EXPECT().DiffFiles("head-hash").Return(nil, nil, nil, nil)

	// No dirty facts → session completes immediately.
	ri.EXPECT().CompletePipelineSession("sess-wm").Return(nil)
	gs.EXPECT().HeadCommit().Return("head-hash", nil)
	ri.EXPECT().SetPipelineWatermark("review", "machine/test", "head-hash").Return(nil)
	ri.EXPECT().PipelineWorkItemStats("sess-wm").Return(0, 0, nil)

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
	ri := NewMockPipelineIndex(ctrl)

	gs.EXPECT().Branch().Return("machine/test")
	ri.EXPECT().GCPipelineSessions("review", "machine/test", 5).Return(nil)
	ri.EXPECT().CreatePipelineSession("review", "machine/test").Return(&store.PipelineSession{
		ID: "sess-2", Branch: "machine/test", Status: "active",
	}, nil)

	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("", nil)

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
	ri.EXPECT().InsertPipelineWorkItem(gomock.Any()).DoAndReturn(func(item store.PipelineWorkItem) error {
		if item.StepType != "prune" && item.StepType != "distill" {
			t.Errorf("unexpected step type: %s", item.StepType)
		}
		return nil
	}).Times(2)

	// nextItem call.
	ri.EXPECT().NextPipelineWorkItem("sess-2").Return(&store.PipelineWorkItem{
		ID:        1,
		SessionID: "sess-2",
		StepType:  "prune",
		FactsJSON: mustJSON(t, []factForLLM{
			{File: "kb/go/one.md", Title: "Fact one", Body: "Body one."},
			{File: "kb/go/two.md", Title: "Fact two", Body: "Body two."},
		}),
		Priority: 2,
	}, nil)
	ri.EXPECT().PipelineWorkItemStats("sess-2").Return(0, 2, nil)

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
	ri := NewMockPipelineIndex(ctrl)

	gs.EXPECT().Branch().Return("machine/test")
	ri.EXPECT().GCPipelineSessions("review", "machine/test", 5).Return(nil)
	ri.EXPECT().CreatePipelineSession("review", "machine/test").Return(&store.PipelineSession{
		ID: "sess-3", Branch: "machine/test", Status: "active",
	}, nil)

	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("old-hash", nil)

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
	ri.EXPECT().InsertPipelineWorkItem(gomock.Any()).DoAndReturn(func(item store.PipelineWorkItem) error {
		if item.StepType != "prune" {
			t.Errorf("expected prune, got %s", item.StepType)
		}
		return nil
	}).Times(1)

	// nextItem call.
	ri.EXPECT().NextPipelineWorkItem("sess-3").Return(&store.PipelineWorkItem{
		ID:        1,
		SessionID: "sess-3",
		StepType:  "prune",
		FactsJSON: mustJSON(t, []factForLLM{
			{File: "kb/go/one.md", Title: "Fact one", Body: "Body one."},
			{File: "kb/go/two.md", Title: "Fact two", Body: "Body two."},
		}),
		Priority: 2,
	}, nil)
	ri.EXPECT().PipelineWorkItemStats("sess-3").Return(0, 1, nil)

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
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-1").Return(&store.PipelineSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	facts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
		{File: "kb/go/two.md", Title: "Fact two", Body: "Body two.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}
	ri.EXPECT().NextPipelineWorkItem("sess-1").Return(&store.PipelineWorkItem{
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


	ri.EXPECT().SetPipelineWorkItemResponse(int64(1), pruneResp).Return(nil)

	// nextItem: no more items → reflect check → complete session.
	ri.EXPECT().NextPipelineWorkItem("sess-1").Return(nil, nil)
	// findHypothesisTransitions: no watermark → no transitions.
	ri.EXPECT().GetPipelineSession("sess-1").Return(&store.PipelineSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("", nil)
	// completeSession.
	ri.EXPECT().GetPipelineSession("sess-1").Return(&store.PipelineSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)
	ri.EXPECT().CompletePipelineSession("sess-1").Return(nil)
	gs.EXPECT().HeadCommit().Return("new-hash", nil)
	ri.EXPECT().SetPipelineWatermark("review", "machine/test", "new-hash").Return(nil)
	ri.EXPECT().PipelineWorkItemStats("sess-1").Return(1, 0, nil)

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
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-1").Return(&store.PipelineSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	facts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}
	ri.EXPECT().NextPipelineWorkItem("sess-1").Return(&store.PipelineWorkItem{
		ID:        1,
		SessionID: "sess-1",
		StepType:  "prune",
		FactsJSON: mustJSON(t, facts),
		Priority:  2,
	}, nil)

	// Keep everything — no side effects from ApplyPruneDecisions.
	pruneResp := `{"decisions": [{"path": "kb/go/one.md", "action": "keep"}]}`
	ri.EXPECT().SetPipelineWorkItemResponse(int64(1), pruneResp).Return(nil)

	// nextItem: another item waiting.
	nextFacts := []factForLLM{
		{File: "kb/go/three.md", Title: "Fact three", Body: "Body three.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
		{File: "kb/go/four.md", Title: "Fact four", Body: "Body four.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}
	ri.EXPECT().NextPipelineWorkItem("sess-1").Return(&store.PipelineWorkItem{
		ID:        2,
		SessionID: "sess-1",
		StepType:  "distill",
		FactsJSON: mustJSON(t, nextFacts),
		Priority:  0,
	}, nil)
	ri.EXPECT().PipelineWorkItemStats("sess-1").Return(1, 1, nil)

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
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("bad-id").Return(nil, nil)

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
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("done-sess").Return(&store.PipelineSession{
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
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-1").Return(&store.PipelineSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	facts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one."},
	}
	ri.EXPECT().NextPipelineWorkItem("sess-1").Return(&store.PipelineWorkItem{
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
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-1").Return(&store.PipelineSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	facts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
		{File: "kb/go/two.md", Title: "Fact two", Body: "Body two.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}
	ri.EXPECT().NextPipelineWorkItem("sess-1").Return(&store.PipelineWorkItem{
		ID:        2,
		SessionID: "sess-1",
		StepType:  "distill",
		FactsJSON: mustJSON(t, facts),
		Priority:  0,
	}, nil)

	distillResp := `{"synthesize": [{"path": "kb/go/combined.md", "title": "Combined", "body": "Merged insight.", "type": "observation", "domain": ["go"], "confidence": 0.9, "entities": [], "refs": ["kb/go/one.md", "kb/go/two.md"]}], "retract": ["kb/go/one.md"]}`

	// ApplyDistillDecisions: computeWeight reads local .md refs, then write synth (path gets UUID), retract one.md.
	gs.EXPECT().ReadFile("kb/go/one.md").Return(factContent("Fact one", "Body one."), nil)
	gs.EXPECT().ReadFile("kb/go/two.md").Return(factContent("Fact two", "Body two."), nil)
	gs.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("c1", "b1", nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)
	idx.EXPECT().GraphAddDerivedFrom(gomock.Any(), gomock.Any()).Return(nil)

	gs.EXPECT().DeleteFile("kb/go/one.md", gomock.Any(), gomock.Any()).Return("c2", nil)
	idx.EXPECT().Delete("kb/go/one.md").Return(nil)

	// RAPTOR: ScopedCluster on the 1 written fact — searches neighbors, clusters.
	// Single fact → filterSmallClusters removes it → no new work items.
	idx.EXPECT().Search(gomock.Any()).Return(nil, nil).AnyTimes()
	idx.EXPECT().ClusterFacts(1.0, 2).Return(store.ClusterResult{}, fmt.Errorf("no embeddings"))

	ri.EXPECT().SetPipelineWorkItemResponse(int64(2), distillResp).Return(nil)

	// nextItem: done — reflect check finds no transitions.
	ri.EXPECT().NextPipelineWorkItem("sess-1").Return(nil, nil)
	ri.EXPECT().GetPipelineSession("sess-1").Return(&store.PipelineSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("", nil)
	// completeSession.
	ri.EXPECT().GetPipelineSession("sess-1").Return(&store.PipelineSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)
	ri.EXPECT().CompletePipelineSession("sess-1").Return(nil)
	gs.EXPECT().HeadCommit().Return("new-hash", nil)
	ri.EXPECT().SetPipelineWatermark("review", "machine/test", "new-hash").Return(nil)
	ri.EXPECT().PipelineWorkItemStats("sess-1").Return(1, 0, nil)

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
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-1").Return(&store.PipelineSession{
		ID: "sess-1", Branch: "machine/test", Status: "active",
	}, nil)

	facts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one."},
	}
	ri.EXPECT().NextPipelineWorkItem("sess-1").Return(&store.PipelineWorkItem{
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
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("", nil)
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
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("abc123", nil)
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
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("abc123", nil)
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

func TestContinueSession_RAPTOR_EnqueuesDeeper(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-r").Return(&store.PipelineSession{
		ID: "sess-r", Branch: "machine/test", Status: "active",
	}, nil)

	inputFacts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
		{File: "kb/go/two.md", Title: "Fact two", Body: "Body two.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
		{File: "kb/go/three.md", Title: "Fact three", Body: "Body three.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}

	// Current work item is distill at depth 0.
	ri.EXPECT().NextPipelineWorkItem("sess-r").Return(&store.PipelineWorkItem{
		ID:        1,
		SessionID: "sess-r",
		StepType:  "distill",
		FactsJSON: mustJSON(t, inputFacts),
		Priority:  0,
		Depth:     0,
	}, nil)

	// Distill response synthesizes 2 new facts (enough for a cluster after ScopedCluster).
	distillResp := `{"synthesize": [` +
		`{"path": "kb/go/synth-a.md", "title": "Synth A", "body": "Synthesized A.", "type": "insight", "domain": ["go"], "confidence": 0.95, "entities": ["Go"], "refs": ["kb/go/one.md"]},` +
		`{"path": "kb/go/synth-b.md", "title": "Synth B", "body": "Synthesized B.", "type": "insight", "domain": ["go"], "confidence": 0.90, "entities": ["Go"], "refs": ["kb/go/two.md"]}` +
		`], "retract": ["kb/go/three.md"]}`

	// ApplyDistillDecisions: computeWeight reads local .md refs per fact, then write 2 synth facts, retract one.
	gs.EXPECT().ReadFile("kb/go/one.md").Return(factContent("Fact one", "Body one."), nil)
	gs.EXPECT().ReadFile("kb/go/two.md").Return(factContent("Fact two", "Body two."), nil)
	gs.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("c1", "b1", nil).Times(2)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil).Times(2)
	idx.EXPECT().GraphAddDerivedFrom(gomock.Any(), gomock.Any()).Return(nil).Times(2)

	gs.EXPECT().DeleteFile("kb/go/three.md", gomock.Any(), gomock.Any()).Return("c2", nil)
	idx.EXPECT().Delete("kb/go/three.md").Return(nil)

	// RAPTOR: ScopedCluster on the 2 written facts.
	// Search returns neighbors so cluster has >1 fact.
	idx.EXPECT().Search(gomock.Any()).Return(nil, nil).AnyTimes()
	// ClusterFacts fails → fallback to category grouping.
	// Both written facts share kb/go → one cluster of 2 facts.
	idx.EXPECT().ClusterFacts(1.0, 2).Return(store.ClusterResult{}, fmt.Errorf("no embeddings"))

	// Expect one new work item inserted at depth 1.
	var insertedItem store.PipelineWorkItem
	ri.EXPECT().InsertPipelineWorkItem(gomock.Any()).DoAndReturn(func(item store.PipelineWorkItem) error {
		insertedItem = item
		return nil
	}).Times(1)

	ri.EXPECT().SetPipelineWorkItemResponse(int64(1), distillResp).Return(nil)

	// nextItem: return the RAPTOR item so session is NOT done.
	ri.EXPECT().NextPipelineWorkItem("sess-r").Return(&store.PipelineWorkItem{
		ID:        2,
		SessionID: "sess-r",
		StepType:  "distill",
		ClusterKey: "raptor-d1-c0",
		FactsJSON: mustJSON(t, inputFacts[:2]), // placeholder
		Priority:  -1,
		Depth:     1,
	}, nil)
	ri.EXPECT().PipelineWorkItemStats("sess-r").Return(1, 1, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.ContinueSession("sess-r", distillResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Session should NOT be done — there are remaining RAPTOR items.
	if result.Done {
		t.Error("expected Done=false, RAPTOR items should remain")
	}
	if result.Progress.Remaining != 1 {
		t.Errorf("expected 1 remaining, got %d", result.Progress.Remaining)
	}

	// Verify the inserted work item has correct depth and type.
	if insertedItem.StepType != "distill" {
		t.Errorf("expected distill step type, got %s", insertedItem.StepType)
	}
	if insertedItem.Depth != 1 {
		t.Errorf("expected depth 1, got %d", insertedItem.Depth)
	}
	if insertedItem.SessionID != "sess-r" {
		t.Errorf("expected session sess-r, got %s", insertedItem.SessionID)
	}
	if insertedItem.ClusterKey != "raptor-d1-c0" {
		t.Errorf("expected cluster key raptor-d1-c0, got %s", insertedItem.ClusterKey)
	}
	if insertedItem.Priority != -1 {
		t.Errorf("expected priority -1, got %f", insertedItem.Priority)
	}
}

func TestContinueSession_RAPTOR_StopsAtMaxDepth(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-max").Return(&store.PipelineSession{
		ID: "sess-max", Branch: "machine/test", Status: "active",
	}, nil)

	inputFacts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
		{File: "kb/go/two.md", Title: "Fact two", Body: "Body two.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}

	// Work item at max depth (3) — RAPTOR should NOT enqueue deeper.
	ri.EXPECT().NextPipelineWorkItem("sess-max").Return(&store.PipelineWorkItem{
		ID:        5,
		SessionID: "sess-max",
		StepType:  "distill",
		FactsJSON: mustJSON(t, inputFacts),
		Priority:  -3,
		Depth:     3,
	}, nil)

	distillResp := `{"synthesize": [{"path": "kb/go/deep.md", "title": "Deep", "body": "Deep synthesis.", "type": "insight", "domain": ["go"], "confidence": 0.95, "entities": [], "refs": []}], "retract": []}`

	// ApplyDistillDecisions writes the synth fact.
	gs.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("c1", "b1", nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)

	// No InsertWorkItem expected — max depth reached.

	ri.EXPECT().SetPipelineWorkItemResponse(int64(5), distillResp).Return(nil)

	// nextItem: done — reflect check finds no transitions.
	ri.EXPECT().NextPipelineWorkItem("sess-max").Return(nil, nil)
	ri.EXPECT().GetPipelineSession("sess-max").Return(&store.PipelineSession{
		ID: "sess-max", Branch: "machine/test", Status: "active",
	}, nil)
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("", nil)
	// completeSession.
	ri.EXPECT().GetPipelineSession("sess-max").Return(&store.PipelineSession{
		ID: "sess-max", Branch: "machine/test", Status: "active",
	}, nil)
	ri.EXPECT().CompletePipelineSession("sess-max").Return(nil)
	gs.EXPECT().HeadCommit().Return("final-hash", nil)
	ri.EXPECT().SetPipelineWatermark("review", "machine/test", "final-hash").Return(nil)
	ri.EXPECT().PipelineWorkItemStats("sess-max").Return(1, 0, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.ContinueSession("sess-max", distillResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Error("expected Done=true at max depth")
	}
}

func TestRunAll_ProcessesAllWorkItems(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockPipelineIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)

	// --- StartSession setup ---
	gs.EXPECT().Branch().Return("machine/test")
	ri.EXPECT().GCPipelineSessions("review", "machine/test", 5).Return(nil)
	ri.EXPECT().CreatePipelineSession("review", "machine/test").Return(&store.PipelineSession{
		ID: "sess-run", Branch: "machine/test", Status: "active",
	}, nil)

	// No watermark → all facts dirty via index.
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("", nil)
	idx.EXPECT().Search(store.SearchQuery{Limit: 100_000}).Return([]store.SearchResult{
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/one.md", Title: "Fact one", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1}, Body: "Body one."}},
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/two.md", Title: "Fact two", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1}, Body: "Body two."}},
	}, nil)

	// ScopedCluster: search + cluster.
	idx.EXPECT().Search(gomock.Any()).Return(nil, nil).AnyTimes()
	idx.EXPECT().ClusterFacts(1.0, 2).Return(store.ClusterResult{}, fmt.Errorf("no embeddings")).AnyTimes()

	// Insert prune + distill work items (2 seeds, 1 cluster of 2).
	ri.EXPECT().InsertPipelineWorkItem(gomock.Any()).Return(nil).Times(2)

	// --- First nextItem: prune ---
	pruneFacts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
		{File: "kb/go/two.md", Title: "Fact two", Body: "Body two.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}
	pruneItem := &store.PipelineWorkItem{
		ID: 1, SessionID: "sess-run", StepType: "prune",
		FactsJSON: mustJSON(t, pruneFacts), Priority: 2,
	}

	distillFacts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
		{File: "kb/go/two.md", Title: "Fact two", Body: "Body two.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}
	distillItem := &store.PipelineWorkItem{
		ID: 2, SessionID: "sess-run", StepType: "distill",
		FactsJSON: mustJSON(t, distillFacts), Priority: 0,
	}

	// NextWorkItem calls: StartSession(1st), ContinueSession-prune(current+next), ContinueSession-distill(current+next=nil)
	nextWorkItemCall := ri.EXPECT().NextPipelineWorkItem("sess-run")
	nextWorkItemCall.Return(pruneItem, nil) // StartSession → first item

	// ContinueSession for prune: first call gets current item, second gets next item
	ri.EXPECT().GetPipelineSession("sess-run").Return(&store.PipelineSession{
		ID: "sess-run", Branch: "machine/test", Status: "active",
	}, nil).AnyTimes()

	// After first nextItem in StartSession, ContinueSession calls NextWorkItem twice:
	// once to get current item to process, once to get next item.
	ri.EXPECT().NextPipelineWorkItem("sess-run").Return(pruneItem, nil)  // ContinueSession: current item
	ri.EXPECT().PipelineWorkItemStats("sess-run").Return(0, 2, nil)       // StartSession stats

	pruneResp := `{"decisions": [{"path": "kb/go/one.md", "action": "keep"}, {"path": "kb/go/two.md", "action": "keep"}]}`
	ri.EXPECT().SetPipelineWorkItemResponse(int64(1), pruneResp).Return(nil)
	ri.EXPECT().NextPipelineWorkItem("sess-run").Return(distillItem, nil) // after prune: next is distill
	ri.EXPECT().PipelineWorkItemStats("sess-run").Return(1, 1, nil)

	// LLM call 1: prune
	adapter.EXPECT().Complete(
		gomock.Any(), "", gomock.Any(),
		llm.CompletionOptions{ForceJSON: true}, nil,
	).Return(pruneResp, nil)

	// ContinueSession for distill
	ri.EXPECT().NextPipelineWorkItem("sess-run").Return(distillItem, nil) // current distill item

	distillResp := `{"synthesize": [], "retract": []}`
	ri.EXPECT().SetPipelineWorkItemResponse(int64(2), distillResp).Return(nil)

	// After distill: no more items → reflect check (no watermark → no transitions) → complete session.
	ri.EXPECT().NextPipelineWorkItem("sess-run").Return(nil, nil)
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("", nil)
	ri.EXPECT().CompletePipelineSession("sess-run").Return(nil)
	gs.EXPECT().HeadCommit().Return("final-hash", nil)
	ri.EXPECT().SetPipelineWatermark("review", "machine/test", "final-hash").Return(nil)
	ri.EXPECT().PipelineWorkItemStats("sess-run").Return(2, 0, nil)

	// LLM call 2: distill
	adapter.EXPECT().Complete(
		gomock.Any(), "", gomock.Any(),
		llm.CompletionOptions{ForceJSON: true}, nil,
	).Return(distillResp, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	err := r.RunAll(context.Background(), adapter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunAll_NoDirtyFacts(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockPipelineIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)

	gs.EXPECT().Branch().Return("machine/test")
	ri.EXPECT().GCPipelineSessions("review", "machine/test", 5).Return(nil)
	ri.EXPECT().CreatePipelineSession("review", "machine/test").Return(&store.PipelineSession{
		ID: "sess-empty", Branch: "machine/test", Status: "active",
	}, nil)

	// No watermark, index returns empty → no dirty facts.
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("", nil)
	idx.EXPECT().Search(store.SearchQuery{Limit: 100_000}).Return(nil, nil)

	// Complete session immediately.
	ri.EXPECT().CompletePipelineSession("sess-empty").Return(nil)
	gs.EXPECT().HeadCommit().Return("abc123", nil)
	ri.EXPECT().SetPipelineWatermark("review", "machine/test", "abc123").Return(nil)
	ri.EXPECT().PipelineWorkItemStats("sess-empty").Return(0, 0, nil)

	// adapter.Complete should NOT be called at all.

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	err := r.RunAll(context.Background(), adapter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReflectStepNotCreatedWhenNoHypotheses(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-noh").Return(&store.PipelineSession{
		ID: "sess-noh", Branch: "machine/test", Status: "active",
	}, nil).AnyTimes()

	// Current work item: prune with observation-only facts.
	facts := []factForLLM{
		{File: "kb/go/one.md", Title: "Fact one", Body: "Body one.", Type: "observation", Domain: []string{"go"}, Confidence: 0.8, Sources: 1},
	}
	ri.EXPECT().NextPipelineWorkItem("sess-noh").Return(&store.PipelineWorkItem{
		ID:        1,
		SessionID: "sess-noh",
		StepType:  "prune",
		FactsJSON: mustJSON(t, facts),
		Priority:  1,
	}, nil)

	// Prune response keeps everything.
	pruneResp := `{"decisions": [{"path": "kb/go/one.md", "action": "keep"}]}`
	ri.EXPECT().SetPipelineWorkItemResponse(int64(1), pruneResp).Return(nil)

	// nextItem: no more items → reflect check.
	ri.EXPECT().NextPipelineWorkItem("sess-noh").Return(nil, nil)
	// findHypothesisTransitions: watermark exists, DiffFiles returns only observation deletes.
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("old-hash", nil)
	gs.EXPECT().DiffFiles("old-hash").Return(nil, nil, []string{"kb/go/obs.md"}, nil)
	// Deleted file was an observation, not a hypothesis.
	gs.EXPECT().ReadFileAtCommit("kb/go/obs.md", "old-hash").Return(
		"---\ntype: observation\ndomain: [go]\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n---\n# Observation\n\nJust an observation.\n", nil)

	// No reflect item should be inserted — session completes directly.
	ri.EXPECT().CompletePipelineSession("sess-noh").Return(nil)
	gs.EXPECT().HeadCommit().Return("new-hash", nil)
	ri.EXPECT().SetPipelineWatermark("review", "machine/test", "new-hash").Return(nil)
	ri.EXPECT().PipelineWorkItemStats("sess-noh").Return(1, 0, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.ContinueSession("sess-noh", pruneResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Error("expected Done=true — no hypothesis transitions means no reflect step")
	}
}

func TestReflectStepCreatedWhenHypothesesRetracted(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-hyp").Return(&store.PipelineSession{
		ID: "sess-hyp", Branch: "machine/test", Status: "active",
	}, nil).AnyTimes()

	// Current work item: prune that retracts a hypothesis.
	facts := []factForLLM{
		{File: "kb/go/hyp.md", Title: "Hypothesis", Body: "A hypothesis.", Type: "hypothesis", Domain: []string{"go"}, Confidence: 0.6, Sources: 1},
	}
	ri.EXPECT().NextPipelineWorkItem("sess-hyp").Return(&store.PipelineWorkItem{
		ID:        1,
		SessionID: "sess-hyp",
		StepType:  "prune",
		FactsJSON: mustJSON(t, facts),
		Priority:  1,
	}, nil)

	pruneResp := `{"decisions": [{"path": "kb/go/hyp.md", "action": "retract"}]}`

	// ApplyPruneDecisions: retract hyp.md.
	gs.EXPECT().DeleteFile("kb/go/hyp.md", gomock.Any(), gomock.Any()).Return("c1", nil)
	idx.EXPECT().Delete("kb/go/hyp.md").Return(nil)

	ri.EXPECT().SetPipelineWorkItemResponse(int64(1), pruneResp).Return(nil)

	// nextItem: no more items → reflect check.
	ri.EXPECT().NextPipelineWorkItem("sess-hyp").Return(nil, nil)
	// findHypothesisTransitions: watermark exists, DiffFiles shows hyp.md deleted.
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("old-hash", nil)
	gs.EXPECT().DiffFiles("old-hash").Return(nil, nil, []string{"kb/go/hyp.md"}, nil)
	// Read the old version — it was a hypothesis.
	gs.EXPECT().ReadFileAtCommit("kb/go/hyp.md", "old-hash").Return(
		"---\ntype: hypothesis\ndomain: [go]\nconfidence: 0.6\nsources: 1\nentities: []\nrefs: []\n---\n# Hypothesis\n\nA hypothesis.\n", nil)

	// Reflect item should be enqueued.
	var insertedReflect store.PipelineWorkItem
	ri.EXPECT().InsertPipelineWorkItem(gomock.Any()).DoAndReturn(func(item store.PipelineWorkItem) error {
		insertedReflect = item
		return nil
	}).Times(1)

	// Recursive nextItem call fetches the reflect item.
	ri.EXPECT().NextPipelineWorkItem("sess-hyp").Return(&store.PipelineWorkItem{
		ID:         10,
		SessionID:  "sess-hyp",
		StepType:   "reflect",
		ClusterKey: "reflect",
		FactsJSON:  `[{"path":"kb/go/hyp.md","original_type":"hypothesis","action":"retracted"}]`,
		Priority:   -100,
	}, nil)
	ri.EXPECT().PipelineWorkItemStats("sess-hyp").Return(1, 1, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.ContinueSession("sess-hyp", pruneResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Done {
		t.Error("expected Done=false — reflect item should be pending")
	}
	if result.Item == nil {
		t.Fatal("expected non-nil Item")
	}
	if result.Item.Type != "reflect" {
		t.Errorf("expected reflect item, got %s", result.Item.Type)
	}

	// Verify the enqueued reflect item.
	if insertedReflect.StepType != "reflect" {
		t.Errorf("expected reflect step type, got %s", insertedReflect.StepType)
	}
	if insertedReflect.Priority != -100 {
		t.Errorf("expected priority -100, got %f", insertedReflect.Priority)
	}
	if insertedReflect.SessionID != "sess-hyp" {
		t.Errorf("expected session sess-hyp, got %s", insertedReflect.SessionID)
	}
}

func TestReflectStepCreatedWhenHypothesisPromoted(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-prom").Return(&store.PipelineSession{
		ID: "sess-prom", Branch: "machine/test", Status: "active",
	}, nil).AnyTimes()

	// Current work item: prune that keeps everything.
	facts := []factForLLM{
		{File: "kb/go/hyp.md", Title: "Hypothesis", Body: "Was a hypothesis.", Type: "observation", Domain: []string{"go"}, Confidence: 0.9, Sources: 1},
	}
	ri.EXPECT().NextPipelineWorkItem("sess-prom").Return(&store.PipelineWorkItem{
		ID:        1,
		SessionID: "sess-prom",
		StepType:  "prune",
		FactsJSON: mustJSON(t, facts),
		Priority:  1,
	}, nil)

	pruneResp := `{"decisions": [{"path": "kb/go/hyp.md", "action": "keep"}]}`
	ri.EXPECT().SetPipelineWorkItemResponse(int64(1), pruneResp).Return(nil)

	// nextItem: no more items → reflect check.
	ri.EXPECT().NextPipelineWorkItem("sess-prom").Return(nil, nil)
	// findHypothesisTransitions: watermark exists, DiffFiles shows hyp.md modified.
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("old-hash", nil)
	gs.EXPECT().DiffFiles("old-hash").Return(nil, []string{"kb/go/hyp.md"}, nil, nil)
	// Old version was hypothesis.
	gs.EXPECT().ReadFileAtCommit("kb/go/hyp.md", "old-hash").Return(
		"---\ntype: hypothesis\ndomain: [go]\nconfidence: 0.6\nsources: 1\nentities: []\nrefs: []\n---\n# Hypothesis\n\nWas a hypothesis.\n", nil)
	// New version is observation (promoted).
	gs.EXPECT().ReadFile("kb/go/hyp.md").Return(
		"---\ntype: observation\ndomain: [go]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Hypothesis\n\nNow an observation.\n", nil)

	// Reflect item should be enqueued.
	ri.EXPECT().InsertPipelineWorkItem(gomock.Any()).DoAndReturn(func(item store.PipelineWorkItem) error {
		if item.StepType != "reflect" {
			t.Errorf("expected reflect, got %s", item.StepType)
		}
		return nil
	}).Times(1)

	// Recursive nextItem fetches the reflect item.
	ri.EXPECT().NextPipelineWorkItem("sess-prom").Return(&store.PipelineWorkItem{
		ID:         10,
		SessionID:  "sess-prom",
		StepType:   "reflect",
		ClusterKey: "reflect",
		FactsJSON:  `[{"path":"kb/go/hyp.md","original_type":"hypothesis","action":"promoted","detail":"type changed to observation"}]`,
		Priority:   -100,
	}, nil)
	ri.EXPECT().PipelineWorkItemStats("sess-prom").Return(1, 1, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.ContinueSession("sess-prom", pruneResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Done {
		t.Error("expected Done=false — reflect item should be pending")
	}
	if result.Item == nil {
		t.Fatal("expected non-nil Item")
	}
	if result.Item.Type != "reflect" {
		t.Errorf("expected reflect item, got %s", result.Item.Type)
	}
}

func TestContinueSession_ReflectResponseCompletesSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-ref").Return(&store.PipelineSession{
		ID: "sess-ref", Branch: "machine/test", Status: "active",
	}, nil).AnyTimes()

	// Current work item is reflect.
	ri.EXPECT().NextPipelineWorkItem("sess-ref").Return(&store.PipelineWorkItem{
		ID:         5,
		SessionID:  "sess-ref",
		StepType:   "reflect",
		ClusterKey: "reflect",
		FactsJSON:  `[{"path":"kb/go/hyp.md","original_type":"hypothesis","action":"retracted"}]`,
		Priority:   -100,
	}, nil)

	reflectResp := `{"methodology_facts": [{"title": "Test hypotheses early", "body": "Validate hypotheses with minimal experiments."}]}`
	ri.EXPECT().SetPipelineWorkItemResponse(int64(5), reflectResp).Return(nil)

	// nextItem after reflect: no more items → reflect already checked → complete session.
	ri.EXPECT().NextPipelineWorkItem("sess-ref").Return(nil, nil)
	// findHypothesisTransitions (first time for this session).
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("", nil)
	// completeSession.
	ri.EXPECT().CompletePipelineSession("sess-ref").Return(nil)
	gs.EXPECT().HeadCommit().Return("final-hash", nil)
	ri.EXPECT().SetPipelineWatermark("review", "machine/test", "final-hash").Return(nil)
	ri.EXPECT().PipelineWorkItemStats("sess-ref").Return(1, 0, nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	result, err := r.ContinueSession("sess-ref", reflectResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Error("expected Done=true after reflect response")
	}
}

func TestFindHypothesisTransitions_ConfidenceUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	ri := NewMockPipelineIndex(ctrl)

	ri.EXPECT().GetPipelineSession("sess-conf").Return(&store.PipelineSession{
		ID: "sess-conf", Branch: "machine/test", Status: "active",
	}, nil)
	ri.EXPECT().GetPipelineWatermark("review", "machine/test").Return("old-hash", nil)
	gs.EXPECT().DiffFiles("old-hash").Return(nil, []string{"kb/go/hyp.md"}, nil, nil)
	// Old version: hypothesis with confidence 0.5.
	gs.EXPECT().ReadFileAtCommit("kb/go/hyp.md", "old-hash").Return(
		"---\ntype: hypothesis\ndomain: [go]\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# Hyp\n\nBody.\n", nil)
	// New version: still hypothesis but confidence changed to 0.8.
	gs.EXPECT().ReadFile("kb/go/hyp.md").Return(
		"---\ntype: hypothesis\ndomain: [go]\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n---\n# Hyp\n\nBody.\n", nil)

	r := NewReviewer(gs, idx, ri, NewMockEmbedder(ctrl), nil)
	transitions, err := r.findHypothesisTransitions("sess-conf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(transitions))
	}
	if transitions[0].Action != "confidence-updated" {
		t.Errorf("expected action confidence-updated, got %s", transitions[0].Action)
	}
	if transitions[0].Path != "kb/go/hyp.md" {
		t.Errorf("expected path kb/go/hyp.md, got %s", transitions[0].Path)
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
