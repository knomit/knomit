package synthesize

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"
)

func TestSumProductNorm(t *testing.T) {
	s := SumProductNorm{}
	cases := []struct {
		srcs []SourceWeight
		want float64
	}{
		{nil, 0},
		{[]SourceWeight{}, 0},
		// sum = 0.8*2 = 1.6; weight = 1.6/2.6 ≈ 0.615
		{[]SourceWeight{{Confidence: 0.8, Sources: 2}}, 1.6 / 2.6},
		// sum = 0.9*3 + 0.6*5 = 2.7+3.0 = 5.7; weight = 5.7/6.7 ≈ 0.851
		{[]SourceWeight{{0.9, 3}, {0.6, 5}}, 5.7 / 6.7},
	}
	for _, c := range cases {
		got := s.Compute(c.srcs)
		if abs(got-c.want) > 1e-9 {
			t.Errorf("Compute(%v) = %v, want %v", c.srcs, got, c.want)
		}
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestSumProductNormBasic(t *testing.T) {
	s := SumProductNorm{}
	got := s.Compute([]SourceWeight{{Confidence: 0.8, Sources: 2}})
	want := 1.6 / 2.6
	if got < want-0.001 || got > want+0.001 {
		t.Errorf("SumProductNorm: got %f want %f", got, want)
	}
}

func TestSumProductNormEmpty(t *testing.T) {
	s := SumProductNorm{}
	if got := s.Compute(nil); got != 0 {
		t.Errorf("SumProductNorm(nil): got %f want 0", got)
	}
}

func TestComputeWeightSkipsHypothesis(t *testing.T) {
	hypContent := "---\ndomain: []\nconfidence: 0.9\nsources: 5\nentities: []\nrefs: []\ntype: hypothesis\n---\n# Hyp\n\nBody."
	obsContent := "---\ndomain: []\nconfidence: 0.8\nsources: 2\nentities: []\nrefs: []\ntype: observation\n---\n# Obs\n\nBody."
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ReadFile(gomock.Any(), "machine/test", "kb/hyp.md").Return(hypContent, nil)
	gs.EXPECT().ReadFile(gomock.Any(), "machine/test", "kb/obs.md").Return(obsContent, nil)
	w := computeWeight(context.Background(), gs, "machine/test", []string{"kb/hyp.md", "kb/obs.md"})
	// Only the observation contributes: sum = 0.8*2 = 1.6; weight = 1.6/2.6
	want := 1.6 / 2.6
	if abs(w-want) > 1e-9 {
		t.Errorf("hypothesis should be skipped: got %v, want %v", w, want)
	}
}

func TestComputeWeight_AllMissing(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ReadFile(gomock.Any(), "machine/test", "kb/missing.md").Return("", fmt.Errorf("not found"))
	w := computeWeight(context.Background(), gs, "machine/test", []string{"kb/missing.md"})
	if w != 0 {
		t.Fatalf("expected 0 for missing sources, got %v", w)
	}
}

func TestComputeWeight_PartialMissing(t *testing.T) {
	content := "---\ndomain: []\nconfidence: 0.8\nsources: 3\nentities: []\nrefs: []\n---\n# Source\n\nBody."
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ReadFile(gomock.Any(), "machine/test", "kb/missing.md").Return("", fmt.Errorf("not found"))
	gs.EXPECT().ReadFile(gomock.Any(), "machine/test", "kb/present.md").Return(content, nil)
	w := computeWeight(context.Background(), gs, "machine/test", []string{"kb/missing.md", "kb/present.md"})
	// sum = 0.8*3 = 2.4; weight = 2.4/3.4
	want := 2.4 / 3.4
	if abs(w-want) > 1e-9 {
		t.Fatalf("weight: got %v, want %v", w, want)
	}
}
