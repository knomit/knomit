package manage

import (
	"encoding/json"
	"strings"
	"testing"
)

// The wire mapping, pinned directly: an INDEXING row carries the heal's own
// counts, and a row with no index state carries none of the three fields.
//
// A unit test on the struct rather than an end-to-end one, because the states
// it has to distinguish are not reachable from this package — markIndexing and
// setIndexProgress are unexported in internal/corpus/repos, and driving a real heal
// into a known partial state is a race, not a fixture. What this package
// actually owns is the mapping, and that is what this asserts.
func TestRepoSummary_IndexFieldsOnTheWire(t *testing.T) {
	indexing, err := json.Marshal(repoSummary{
		Name: "busy", State: repoStateActive,
		IndexState: "indexing", IndexDone: 7, IndexTotal: 31,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"index_state":"indexing"`, `"index_done":7`, `"index_total":31`} {
		if !strings.Contains(string(indexing), want) {
			t.Fatalf("missing %s in %s", want, indexing)
		}
	}

	// An unavailable row has no store to ask, so it reports nothing rather
	// than "ready" — the same reason such a row carries no id.
	unavailable, err := json.Marshal(repoSummary{Name: "broken", State: "missing", Detail: "no database"})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"index_state", "index_done", "index_total"} {
		if strings.Contains(string(unavailable), absent) {
			t.Fatalf("a row with no index state must not carry %s: %s", absent, unavailable)
		}
	}

	// And a READY row carries the state without counts: "0/0" on a ready repo
	// would read as a claim about it rather than as an absence.
	ready, err := json.Marshal(repoSummary{Name: "done", State: repoStateActive, IndexState: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ready), `"index_state":"ready"`) {
		t.Fatalf("ready row lost its state: %s", ready)
	}
	for _, absent := range []string{"index_done", "index_total"} {
		if strings.Contains(string(ready), absent) {
			t.Fatalf("a ready row must not carry %s: %s", absent, ready)
		}
	}
}
