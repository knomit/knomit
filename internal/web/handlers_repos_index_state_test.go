package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every ACTIVE row carries its index state, not just the repo page for the one
// open repo. A repo mid-heal answers queries partially, and a list that says
// nothing about that makes partial results look like missing knowledge.
func TestGetRepos_RowsCarryIndexState(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	id := startCreate(t, r, "indexed")
	awaitCreateID(t, r, id)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	row := repoRow(t, rec.Body.Bytes(), "indexed")
	// The create's own job reported done only once the heal left 'indexing'
	// (mirrorIndexing), so a finished create means a ready row.
	if row["index_state"] != "ready" {
		t.Fatalf("index_state = %v, want ready: %v", row["index_state"], row)
	}
	// A ready row carries no counts: "0/0" on a ready repo would read as a
	// claim about it rather than as an absence.
	if _, ok := row["index_done"]; ok {
		t.Fatalf("a ready row must not carry index_done: %v", row)
	}
	if _, ok := row["index_total"]; ok {
		t.Fatalf("a ready row must not carry index_total: %v", row)
	}

	// The field really comes from the instance, not from a constant: ask the
	// manager the same question and require the same answer.
	ri := s.Manager.Get("indexed")
	if ri == nil {
		t.Fatal("repo not registered")
	}
	state, _, _ := ri.IndexStatus()
	if row["index_state"] != state {
		t.Fatalf("row says %v, the instance says %q", row["index_state"], state)
	}
}

// The wire mapping, pinned directly: an INDEXING row carries the heal's own
// counts, and a row with no index state carries none of the three fields.
//
// A unit test on the struct rather than an end-to-end one, because the states
// it has to distinguish are not reachable from this package — markIndexing and
// setIndexProgress are unexported in internal/repos, and driving a real heal
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

func repoRow(t *testing.T, raw []byte, name string) map[string]any {
	t.Helper()
	var body struct {
		Embedded struct {
			Repos []map[string]any `json:"repos"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("bad collection body %q: %v", raw, err)
	}
	for _, row := range body.Embedded.Repos {
		if row["name"] == name {
			return row
		}
	}
	t.Fatalf("no row named %q in %s", name, raw)
	return nil
}
