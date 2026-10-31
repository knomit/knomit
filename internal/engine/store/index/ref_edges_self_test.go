package index

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// batchSelfRepo counts only the blobs with exactly one candidate (PR #694
// delta review D1): an ambiguous blob neither votes nor vetoes, and blobs
// that disagree leave the batch with no id.
func TestBatchSelfRepo(t *testing.T) {
	set := func(rs ...string) map[string]bool {
		m := map[string]bool{}
		for _, r := range rs {
			m[r] = true
		}
		return m
	}
	for _, tc := range []struct {
		name string
		per  []map[string]bool
		want string
	}{
		{"no blobs", nil, ""},
		{"no candidates", []map[string]bool{set(), set()}, ""},
		{"one unambiguous blob", []map[string]bool{set("self")}, "self"},
		{"unambiguous blobs agree", []map[string]bool{set("self"), set(), set("self")}, "self"},
		{"an ambiguous blob does not veto", []map[string]bool{set("self"), set("self", "other")}, "self"},
		{"an ambiguous blob does not vote", []map[string]bool{set("self", "other")}, ""},
		{"ambiguous blobs only", []map[string]bool{set("self", "other"), set("self", "third")}, ""},
		{"unambiguous blobs disagree", []map[string]bool{set("self"), set("other")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, batchSelfRepo(tc.per))
		})
	}
}
