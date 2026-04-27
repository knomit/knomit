package synthesize

import (
	"context"
	"testing"
)

// TestDedupCluster_NoEmbedderDependency regresses the optimization where
// dedupCluster used to take an `embedders ...store.Embedder` variadic and
// run ONNX inference on every cluster member's title+body — even though
// each fact already has a 768-dim vector in facts_vec. The function now
// resolves vectors via SearchQuery.QueryByPath (a SQL subquery in the
// MATCH operand). If anyone re-introduces an embedder dependency, this
// test stops compiling.
//
// Behavioral coverage: a cluster with fewer than 2 members is a no-op —
// dedupCluster returns it unchanged before touching FactIndex,
// SearchIndex, or anything else. That's the cheapest exercise of the new
// signature without needing a real store stood up.
func TestDedupCluster_NoEmbedderDependency(t *testing.T) {
	cases := []struct {
		name    string
		cluster []factForLLM
	}{
		{"empty cluster", nil},
		{"single fact cluster", []factForLLM{{File: "kb/a.md", Title: "a", Body: "b"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := dedupCluster(
				context.Background(),
				tc.cluster,
				nil, // FactIndex — never reached for len(cluster) < 2
				nil, // SearchIndex — never reached
				0.92,
				"test",
				func(ProgressEvent) {},
				"agent/test",
			)
			if err != nil {
				t.Fatalf("dedupCluster err = %v", err)
			}
			if len(out) != len(tc.cluster) {
				t.Errorf("dedupCluster returned %d facts, want %d", len(out), len(tc.cluster))
			}
		})
	}
}
