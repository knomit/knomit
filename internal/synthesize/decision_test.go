package synthesize

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"
	"knomit/internal/store"
)

// factContent builds a minimal knomit fact file for testing.
func factContent(title, body string) string {
	return "---\ndomain: [testing]\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n---\n# " + title + "\n\n" + body + "\n"
}

func TestApplyPruneDecisions_Retract(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().DeleteFact(gomock.Any(), "agent/test", "kb/test/old.md", gomock.Any()).Return("c1", nil)
	idx.EXPECT().Delete(gomock.Any(), gomock.Any(), "kb/test/old.md").Return(nil)


	decisions := []PruneDecision{
		{Path: "kb/test/old.md", Action: "retract"},
	}
	progress := collectProgress()

	stats, err := ApplyPruneDecisions(context.Background(), gs, idx, decisions, nil, "test", progress.fn, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Pruned != 1 {
		t.Errorf("expected Pruned=1, got %d", stats.Pruned)
	}
	if stats.Updated != 0 || stats.Merged != 0 {
		t.Errorf("expected no updates or merges, got Updated=%d Merged=%d", stats.Updated, stats.Merged)
	}
	progress.assertContains(t, "detail-retract")
}

func TestApplyPruneDecisions_Update(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	content := factContent("Test fact", "Some body text.")
	gs.EXPECT().ReadFact(gomock.Any(), "agent/test", "kb/test/upd.md", gomock.Any()).Return(store.ReadFactResult{Content: content}, nil)
	gs.EXPECT().WriteFact(gomock.Any(), "agent/test", "kb/test/upd.md", gomock.Any(), gomock.Any(), gomock.Any()).Return(store.WriteFactResult{CommitHash: "c2", BlobHash: "b2"}, nil)
	idx.EXPECT().Upsert(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, branch, commitHash string, r store.FactRecord) error {
		if r.Confidence != 0.5 {
			t.Errorf("expected confidence 0.5, got %f", r.Confidence)
		}
		return nil
	})


	decisions := []PruneDecision{
		{Path: "kb/test/upd.md", Action: "update", Confidence: 0.5},
	}
	progress := collectProgress()

	stats, err := ApplyPruneDecisions(context.Background(), gs, idx, decisions, nil, "test", progress.fn, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Updated != 1 {
		t.Errorf("expected Updated=1, got %d", stats.Updated)
	}
	progress.assertContains(t, "detail-update")
}

func TestApplyPruneDecisions_Keep(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	// "keep" should produce no side effects — no mock expectations needed.
	decisions := []PruneDecision{
		{Path: "kb/test/keep.md", Action: "keep"},
	}
	progress := collectProgress()

	stats, err := ApplyPruneDecisions(context.Background(), gs, idx, decisions, nil, "test", progress.fn, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Pruned != 0 || stats.Updated != 0 || stats.Merged != 0 {
		t.Errorf("keep should have no stats, got %+v", stats)
	}
}

func TestApplyPruneDecisions_Merge(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	// computeWeight reads sources before writing merged fact.
	gs.EXPECT().ReadFact(gomock.Any(), "agent/test", "kb/test/a.md", gomock.Any()).Return(store.ReadFactResult{Content: factContent("Fact A", "Body A.")}, nil)
	gs.EXPECT().ReadFact(gomock.Any(), "agent/test", "kb/test/b.md", gomock.Any()).Return(store.ReadFactResult{Content: factContent("Fact B", "Body B.")}, nil)
	// Write merged fact.
	gs.EXPECT().WriteFact(gomock.Any(), "agent/test", "kb/test/merged.md", gomock.Any(), gomock.Any(), gomock.Any()).Return(store.WriteFactResult{CommitHash: "c3", BlobHash: "b3"}, nil)
	idx.EXPECT().Upsert(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	// Delete sources.
	gs.EXPECT().DeleteFact(gomock.Any(), "agent/test", "kb/test/a.md", gomock.Any()).Return("c4", nil)
	idx.EXPECT().Delete(gomock.Any(), gomock.Any(), "kb/test/a.md").Return(nil)
	gs.EXPECT().DeleteFact(gomock.Any(), "agent/test", "kb/test/b.md", gomock.Any()).Return("c5", nil)
	idx.EXPECT().Delete(gomock.Any(), gomock.Any(), "kb/test/b.md").Return(nil)


	merges := []MergeEntry{
		{
			Paths: []string{"kb/test/a.md", "kb/test/b.md"},
			Merged: mergedFact{
				Path:       "kb/test/merged.md",
				Title:      "Merged",
				Body:       "Combined.",
				Type:       "observation",
				Domain:     []string{"testing"},
				Confidence: 0.9,
				Entities:   []string{},
				Refs:       []string{},
			},
		},
	}
	progress := collectProgress()

	stats, err := ApplyPruneDecisions(context.Background(), gs, idx, nil, merges, "test", progress.fn, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Merged != 1 {
		t.Errorf("expected Merged=1, got %d", stats.Merged)
	}
	progress.assertContains(t, "detail-merge")
}

func TestApplyPruneDecisions_NoDoubleDelete(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	// Path "kb/test/a.md" appears in both retract decision and merge sources.
	// It should only be deleted once.
	gs.EXPECT().DeleteFact(gomock.Any(), "agent/test", "kb/test/a.md", gomock.Any()).Return("c1", nil).Times(1)
	idx.EXPECT().Delete(gomock.Any(), gomock.Any(), "kb/test/a.md").Return(nil).Times(1)
	// computeWeight reads source before writing merged fact.
	gs.EXPECT().ReadFact(gomock.Any(), "agent/test", "kb/test/a.md", gomock.Any()).Return(store.ReadFactResult{Content: factContent("Fact A", "Body A.")}, nil)
	// Merge write.
	gs.EXPECT().WriteFact(gomock.Any(), "agent/test", "kb/test/merged.md", gomock.Any(), gomock.Any(), gomock.Any()).Return(store.WriteFactResult{CommitHash: "c2", BlobHash: "b2"}, nil)
	idx.EXPECT().Upsert(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

	decisions := []PruneDecision{
		{Path: "kb/test/a.md", Action: "retract"},
	}
	merges := []MergeEntry{
		{
			Paths: []string{"kb/test/a.md"},
			Merged: mergedFact{
				Path:  "kb/test/merged.md",
				Title: "Merged",
				Body:  "Body.",
				Type:  "observation",
			},
		},
	}

	_, err := ApplyPruneDecisions(context.Background(), gs, idx, decisions, merges, "test", func(ProgressEvent) {}, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyDistillDecisions_SynthesizeAndRetract(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	// computeWeight reads local .md refs before writing synthesized fact.
	gs.EXPECT().ReadFact(gomock.Any(), "agent/test", "kb/test/src1.md", gomock.Any()).Return(store.ReadFactResult{Content: factContent("Src 1", "Body 1.")}, nil)
	gs.EXPECT().ReadFact(gomock.Any(), "agent/test", "kb/test/src2.md", gomock.Any()).Return(store.ReadFactResult{Content: factContent("Src 2", "Body 2.")}, nil)
	// Synthesized fact write — path gets a UUID filename, so match on prefix.
	gs.EXPECT().WriteFact(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, branch, path, content, msg, operation string) (store.WriteFactResult, error) {
			if !strings.HasPrefix(path, "kb/test/") || !strings.HasSuffix(path, ".md") {
				t.Errorf("expected path kb/test/<uuid>.md, got %s", path)
			}
			if path == "kb/test/synth.md" {
				t.Errorf("expected UUID filename, got LLM-generated name: %s", path)
			}
			return store.WriteFactResult{CommitHash: "c1", BlobHash: "b1"}, nil
		})
	idx.EXPECT().Upsert(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, branch, commitHash string, r store.FactRecord) error {
		if !strings.HasPrefix(r.Path, "kb/test/") {
			t.Errorf("expected path under kb/test/, got %s", r.Path)
		}
		if r.Sources != 1 {
			t.Errorf("expected sources=1, got %d", r.Sources)
		}
		return nil
	})

	// Retract.
	gs.EXPECT().DeleteFact(gomock.Any(), "agent/test", "kb/test/old.md", gomock.Any()).Return("c2", nil)
	idx.EXPECT().Delete(gomock.Any(), gomock.Any(), "kb/test/old.md").Return(nil)

	synthesized := []distillFact{
		{
			Path:       "kb/test/synth.md",
			Title:      "Synthesized",
			Body:       "Higher-order insight.",
			Type:       "observation",
			Domain:     []string{"testing"},
			Confidence: 0.85,
			Entities:   []string{"test"},
			Refs:       []string{"kb/test/src1.md", "kb/test/src2.md"},
		},
	}
	retract := []string{"kb/test/old.md"}
	progress := collectProgress()

	stats, _, err := ApplyDistillDecisions(context.Background(), gs, idx, synthesized, retract, "test", progress.fn, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Synthesized != 1 {
		t.Errorf("expected Synthesized=1, got %d", stats.Synthesized)
	}
	if stats.Pruned != 1 {
		t.Errorf("expected Pruned=1, got %d", stats.Pruned)
	}
	progress.assertContains(t, "detail-learn")
	progress.assertContains(t, "detail-distill-retract")
}

func TestApplyDistillDecisions_NoRefs(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	// Upsert handles DERIVED_FROM; no separate call needed.
	gs.EXPECT().WriteFact(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(store.WriteFactResult{CommitHash: "c1", BlobHash: "b1"}, nil)
	idx.EXPECT().Upsert(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

	synthesized := []distillFact{
		{
			Path:       "kb/test/synth.md",
			Title:      "No refs",
			Body:       "Body.",
			Type:       "observation",
			Confidence: 0.9,
		},
	}

	stats, _, err := ApplyDistillDecisions(context.Background(), gs, idx, synthesized, nil, "test", func(ProgressEvent) {}, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Synthesized != 1 {
		t.Errorf("expected Synthesized=1, got %d", stats.Synthesized)
	}
}

func TestApplyDistillRejectsHypothesisType(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	// No expectations on gs or idx — hypothesis is skipped before any I/O.

	synthesized := []distillFact{
		{
			Path:       "kb/test/hyp.md",
			Title:      "A hypothesis",
			Body:       "Maybe this is true.",
			Type:       "hypothesis",
			Domain:     []string{"testing"},
			Confidence: 0.7,
		},
	}
	progress := collectProgress()

	stats, written, err := ApplyDistillDecisions(context.Background(), gs, idx, synthesized, nil, "test", progress.fn, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Synthesized != 0 {
		t.Errorf("expected Synthesized=0 (hypothesis skipped), got %d", stats.Synthesized)
	}
	if len(written) != 0 {
		t.Errorf("expected no written facts, got %d", len(written))
	}
	// A warn progress event should have been emitted.
	found := false
	for _, e := range progress.events {
		if e.Phase == "warn" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a warn progress event for rejected hypothesis, got %v", progress.events)
	}
}

// progressCollector is a test helper that records progress events.
type progressCollector struct {
	events []ProgressEvent
	fn     func(ProgressEvent)
}

func collectProgress() *progressCollector {
	pc := &progressCollector{}
	pc.fn = func(e ProgressEvent) {
		pc.events = append(pc.events, e)
	}
	return pc
}

func (pc *progressCollector) assertContains(t *testing.T, phase string) {
	t.Helper()
	for _, e := range pc.events {
		if e.Phase == phase {
			return
		}
	}
	t.Errorf("expected progress event with phase %q, got %v", phase, pc.events)
}
