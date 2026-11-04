package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/storetest"
)

// GET /repos/{name} names the ontology file in force (#429: ontology changes
// that arrive through git are applied live, so "which ontology" is no longer
// "the one at the last mount"): its commit, rung and blob. A changed ontology
// on the read branch replaces it with no restart; an unusable one is not
// applied, and both the single GET (ontology.reload_error) and the list row
// (ontology_reload_error) say why.
//
// Sabotage: drop the `ontology` block from repoView → red at the first check;
// drop OntologyReloadError from summaryFor → red at the list check.
func TestGetRepo_OntologyStatusFollowsTheReadBranch(t *testing.T) {
	const agentBranch = "machine/test"
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "live")
	ri := s.Manager.Get("live")

	blobAt := func() (string, string) {
		t.Helper()
		var c, b string
		var berr error
		if err := ri.WithRead(func(svc *store.Service) {
			c, _, b, berr = svc.OntologyBlobAt(context.Background(), agentBranch)
		}); err != nil || berr != nil {
			t.Fatalf("ontology blob: %v / %v", err, berr)
		}
		return c, b
	}
	ontologyOf := func() map[string]any {
		t.Helper()
		o, _ := getRepoBody(t, r, "live")["ontology"].(map[string]any)
		if o == nil {
			t.Fatalf("GET /repos/live carries no ontology block")
		}
		return o
	}
	write := func(content, msg string) {
		t.Helper()
		var werr error
		if err := ri.WithRead(func(svc *store.Service) {
			_, werr = storetest.RawWrite(context.Background(), svc, agentBranch, fact.OntologyFile, content, msg)
		}); err != nil || werr != nil {
			t.Fatalf("%s: %v / %v", msg, err, werr)
		}
	}

	commit0, blob0 := blobAt()
	o := ontologyOf()
	if o["commit"] != commit0 || o["blob"] != blob0 || o["path"] != fact.OntologyFile {
		t.Fatalf("ontology = %v, want commit %s blob %s path %s", o, commit0, blob0, fact.OntologyFile)
	}

	// A different, usable ontology on the read branch: applied live.
	cur := ri.Ontology()
	y, err := cur.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(y), "topics:\n", "topics:\n  zeta:\n    description: zeta\n", 1)
	write(changed, "add zeta")
	_, blob1 := blobAt()
	waitFor(t, func() bool { return ontologyOf()["blob"] == blob1 }, "the changed ontology's blob in force")
	if _, err := fact.ValidatePlacement(ri.Ontology(), "zeta", "a/b"); err != nil {
		t.Fatalf("zeta not writable after the live reload: %v", err)
	}

	// An unparseable one: the old stays in force, and status says why.
	write("topics: [::\n", "garble")
	waitFor(t, func() bool { _, ok := ontologyOf()["reload_error"]; return ok }, "a reload error")
	o = ontologyOf()
	if o["blob"] != blob1 || !strings.Contains(o["reload_error"].(string), "does not parse") {
		t.Fatalf("ontology = %v, want blob %s in force and a parse reload_error", o, blob1)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos", nil))
	row := repoRow(t, rec.Body.Bytes(), "live")
	if got, _ := row["ontology_reload_error"].(string); !strings.Contains(got, "does not parse") {
		t.Fatalf("list row ontology_reload_error = %q", got)
	}
	if _, ok := row["ontology_error"]; ok {
		t.Fatalf("the repo still has an ontology in force; ontology_error must stay absent: %v", row)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
