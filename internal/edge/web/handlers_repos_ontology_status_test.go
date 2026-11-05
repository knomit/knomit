package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/facts"
	"knomit/internal/engine/store/storetest"
)

// GET /repos/{name} names the ontology file in force (#429: ontology changes
// that arrive through git are applied live, so "which ontology" is no longer
// "the one at the last mount"): its commit, rung and blob. A changed ontology
// on the read branch replaces it with no restart; an unusable one makes the
// repo unwritable (ontology.usable false, ontology.error, the list row's
// ontology_error) and an in-process write fails; the fix restores it.
//
// Sabotage: drop the `ontology` block from repoView → red at the first check;
// keep the old ontology on an unusable file → usable never turns false → red.
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

	// An unparseable one (#429 decision 1): the repo is UNWRITABLE, and both
	// the single GET and the list row say why.
	write("topics: [::\n", "garble")
	waitFor(t, func() bool { return ontologyOf()["usable"] == false }, "the repo to turn unwritable")
	_, blob2 := blobAt()
	o = ontologyOf()
	if o["blob"] != blob2 || !strings.Contains(o["error"].(string), "does not parse") {
		t.Fatalf("ontology = %v, want the garbled blob %s named and a parse error", o, blob2)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos", nil))
	row := repoRow(t, rec.Body.Bytes(), "live")
	if got, _ := row["ontology_error"].(string); !strings.Contains(got, "does not parse") {
		t.Fatalf("list row ontology_error = %q, want the parse error", got)
	}
	if ri.WritableBranch(agentBranch) {
		t.Fatalf("an unusable consensus ontology must make the repo unwritable")
	}
	var werr error
	if err := ri.WithRead(func(svc *store.Service) {
		_, werr = svc.Facts().WriteFact(context.Background(), agentBranch, "kb/zeta/a/b.md",
			"---\ntype: observation\nconfidence: 0.5\n---\n# T\n\nbody\n", "w", "learn")
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(werr, facts.ErrOntologyUnusable) {
		t.Fatalf("an in-process fact write = %v, want ErrOntologyUnusable (no fact is written unjudged)", werr)
	}

	// Consensus fixes it: usable and writable again, no restart.
	write(changed, "fix")
	waitFor(t, func() bool { return ontologyOf()["usable"] == true }, "the repo to turn writable again")
	if !ri.WritableBranch(agentBranch) {
		t.Fatalf("the fixed ontology must make the repo writable again")
	}
	if _, ok := ontologyOf()["error"]; ok {
		t.Fatalf("a usable ontology carries no error: %v", ontologyOf())
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
