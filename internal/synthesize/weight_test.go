package synthesize

import (
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

func TestComputeWeight_AllMissing(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ReadFile("kb/missing.md").Return("", fmt.Errorf("not found"))
	w := computeWeight(gs, []string{"kb/missing.md"})
	if w != 0 {
		t.Fatalf("expected 0 for missing sources, got %v", w)
	}
}

func TestComputeWeight_PartialMissing(t *testing.T) {
	content := "---\ndomain: []\nconfidence: 0.8\nsources: 3\nentities: []\nrefs: []\n---\n# Source\n\nBody."
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ReadFile("kb/missing.md").Return("", fmt.Errorf("not found"))
	gs.EXPECT().ReadFile("kb/present.md").Return(content, nil)
	w := computeWeight(gs, []string{"kb/missing.md", "kb/present.md"})
	// sum = 0.8*3 = 2.4; weight = 2.4/3.4
	want := 2.4 / 3.4
	if abs(w-want) > 1e-9 {
		t.Fatalf("weight: got %v, want %v", w, want)
	}
}
