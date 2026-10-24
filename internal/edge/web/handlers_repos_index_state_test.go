package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	state := ri.Status().Index.State
	if row["index_state"] != state {
		t.Fatalf("row says %v, the instance says %q", row["index_state"], state)
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
