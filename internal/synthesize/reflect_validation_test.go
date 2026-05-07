package synthesize

import (
	"strings"
	"testing"
)

// TestValidateReflectResponse covers the structural validation contract.
// DB-needing checks (reinforce path exists, propose novelty) live in
// ApplyReflectDecisions and are tested separately.
func TestValidateReflectResponse(t *testing.T) {
	transitions := []string{"kb/h1.md", "kb/h2.md"}
	const cap = 1

	validReinforce := ReinforceEntry{
		MethodologyPath: "kb/meta/reasoning/m.md",
		TransitionPaths: []string{"kb/h1.md"},
		Rationale:       "explained by m",
	}
	validPropose := ProposeEntry{
		Title:           "T",
		Body:            "B",
		TopicPath:       "kb/meta/reasoning",
		Confidence:      0.7,
		TransitionPaths: []string{"kb/h2.md"},
		NoveltyArgument: "no existing methodology covers this",
	}

	cases := []struct {
		name   string
		result ReflectResult
		want   string // empty = expect no error; otherwise substring match
	}{
		{
			name:   "empty arrays accepted",
			result: ReflectResult{Reasoning: "looked, found nothing"},
			want:   "",
		},
		{
			name: "single reinforce ok",
			result: ReflectResult{
				Reasoning: "h1 fits m",
				Reinforce: []ReinforceEntry{validReinforce},
			},
			want: "",
		},
		{
			name: "single propose ok",
			result: ReflectResult{
				Reasoning: "h2 is genuinely new",
				Propose:   []ProposeEntry{validPropose},
			},
			want: "",
		},
		{
			name: "propose cap exceeded",
			result: ReflectResult{
				Propose: []ProposeEntry{validPropose, validPropose},
			},
			want: "propose cap",
		},
		{
			name: "reinforce transition path unknown to session",
			result: ReflectResult{
				Reinforce: []ReinforceEntry{{
					MethodologyPath: "kb/meta/reasoning/m.md",
					TransitionPaths: []string{"kb/h-nope.md"},
					Rationale:       "x",
				}},
			},
			want: "kb/h-nope.md",
		},
		{
			name: "reinforce transition_paths empty",
			result: ReflectResult{
				Reinforce: []ReinforceEntry{{
					MethodologyPath: "kb/meta/reasoning/m.md",
					TransitionPaths: []string{},
					Rationale:       "x",
				}},
			},
			want: "transition_paths must be non-empty",
		},
		{
			name: "reinforce missing methodology_path",
			result: ReflectResult{
				Reinforce: []ReinforceEntry{{
					TransitionPaths: []string{"kb/h1.md"},
					Rationale:       "x",
				}},
			},
			want: "methodology_path",
		},
		{
			name: "propose missing title",
			result: ReflectResult{
				Propose: []ProposeEntry{{
					Body:            "B",
					TopicPath:       "kb/meta/reasoning",
					Confidence:      0.7,
					TransitionPaths: []string{"kb/h1.md"},
					NoveltyArgument: "novel",
				}},
			},
			want: "title",
		},
		{
			name: "propose missing body",
			result: ReflectResult{
				Propose: []ProposeEntry{{
					Title:           "T",
					TopicPath:       "kb/meta/reasoning",
					Confidence:      0.7,
					TransitionPaths: []string{"kb/h1.md"},
					NoveltyArgument: "novel",
				}},
			},
			want: "body",
		},
		{
			name: "propose missing topic_path",
			result: ReflectResult{
				Propose: []ProposeEntry{{
					Title:           "T",
					Body:            "B",
					Confidence:      0.7,
					TransitionPaths: []string{"kb/h1.md"},
					NoveltyArgument: "novel",
				}},
			},
			want: "topic_path",
		},
		{
			name: "propose missing novelty_argument",
			result: ReflectResult{
				Propose: []ProposeEntry{{
					Title:           "T",
					Body:            "B",
					TopicPath:       "kb/meta/reasoning",
					Confidence:      0.7,
					TransitionPaths: []string{"kb/h1.md"},
				}},
			},
			want: "novelty_argument",
		},
		{
			name: "propose missing transition_paths",
			result: ReflectResult{
				Propose: []ProposeEntry{{
					Title:           "T",
					Body:            "B",
					TopicPath:       "kb/meta/reasoning",
					Confidence:      0.7,
					NoveltyArgument: "novel",
				}},
			},
			want: "transition_paths",
		},
		{
			name: "propose transition path unknown to session",
			result: ReflectResult{
				Propose: []ProposeEntry{{
					Title:           "T",
					Body:            "B",
					TopicPath:       "kb/meta/reasoning",
					Confidence:      0.7,
					TransitionPaths: []string{"kb/h-nope.md"},
					NoveltyArgument: "novel",
				}},
			},
			want: "kb/h-nope.md",
		},
		{
			name: "propose confidence out of range",
			result: ReflectResult{
				Propose: []ProposeEntry{{
					Title:           "T",
					Body:            "B",
					TopicPath:       "kb/meta/reasoning",
					Confidence:      1.5,
					TransitionPaths: []string{"kb/h1.md"},
					NoveltyArgument: "novel",
				}},
			},
			want: "confidence",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReflectResponse(tc.result, transitions, cap)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected nil err, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain expected substring %q", err.Error(), tc.want)
			}
		})
	}
}

// TestParseReflectResponse_StripsCodeFences regresses the LLM-output
// pattern where models wrap JSON in ```json ... ``` fences.
func TestParseReflectResponse_StripsCodeFences(t *testing.T) {
	raw := "```json\n{\"reasoning\":\"x\",\"reinforce\":[],\"propose\":[]}\n```"
	r, err := parseReflectResponse(raw)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if r.Reasoning != "x" {
		t.Fatalf("reasoning = %q, want %q", r.Reasoning, "x")
	}
}
