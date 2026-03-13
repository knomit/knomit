package synthesize

import "testing"

func TestIsPrunePassive(t *testing.T) {
	tests := []struct {
		name   string
		result PruneResult
		want   bool
	}{
		{
			name: "all keep, no merges — passive",
			result: PruneResult{
				Decisions: []PruneDecision{
					{Path: "a.md", Action: "keep"},
					{Path: "b.md", Action: "keep"},
				},
			},
			want: true,
		},
		{
			name: "has forget — active",
			result: PruneResult{
				Decisions: []PruneDecision{
					{Path: "a.md", Action: "keep"},
					{Path: "b.md", Action: "retract"},
				},
			},
			want: false,
		},
		{
			name: "has update — active",
			result: PruneResult{
				Decisions: []PruneDecision{
					{Path: "a.md", Action: "update", Confidence: 0.5},
				},
			},
			want: false,
		},
		{
			name: "has merge — active",
			result: PruneResult{
				Decisions: []PruneDecision{
					{Path: "a.md", Action: "keep"},
				},
				Merges: []MergeEntry{{Paths: []string{"a.md", "b.md"}}},
			},
			want: false,
		},
		{
			name:   "empty decisions — passive",
			result: PruneResult{},
			want:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPrunePassive(tc.result); got != tc.want {
				t.Errorf("isPrunePassive = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsDistillPassive(t *testing.T) {
	tests := []struct {
		name       string
		result     DistillResult
		inputPaths []string
		want       bool
	}{
		{
			name:       "empty synthesize — passive",
			result:     DistillResult{},
			inputPaths: []string{"a.md"},
			want:       true,
		},
		{
			name: "has new facts and forgets — active",
			result: DistillResult{
				Synthesize: []distillFact{{Path: "new.md", Title: "New"}},
				Retract:     []string{"a.md"},
			},
			inputPaths: []string{"a.md"},
			want:       false,
		},
		{
			name: "synthesized paths match inputs, no forget — passive",
			result: DistillResult{
				Synthesize: []distillFact{
					{Path: "a.md", Title: "Same"},
					{Path: "b.md", Title: "Same"},
				},
			},
			inputPaths: []string{"a.md", "b.md"},
			want:       true,
		},
		{
			name: "synthesized includes new path — active",
			result: DistillResult{
				Synthesize: []distillFact{
					{Path: "new.md", Title: "New insight"},
				},
			},
			inputPaths: []string{"a.md", "b.md"},
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDistillPassive(tc.result, tc.inputPaths); got != tc.want {
				t.Errorf("isDistillPassive = %v, want %v", got, tc.want)
			}
		})
	}
}
