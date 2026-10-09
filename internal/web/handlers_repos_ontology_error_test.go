package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// What GET /repos says about a repo's ontology, for each way it can be wrong
// (user ruling 2026-10-08):
//
//   - a SYMLINKED ontology refuses the repo, and boot archives it (source
//     system, the named symlink message as reason): it leaves GET /repos, is
//     listed in GET /archived, and its URL answers 404 with that reason;
//   - a MISSING, EMPTY or UNPARSEABLE ontology leaves the repo mounted and
//     readable, and both the list row and the single GET carry ontology_error;
//   - a regular ontology carries no ontology_error at all.
//
// Each repo is created through the API, its ontology is then broken on the
// agent branch, and the manager is restarted so Identify reads it.
//
// Sabotage: drop OntologyError from summaryFor, or ontology_error from
// repoView → the missing/empty/garbled checks go red; report the
// not-identified placeholder (OntologyError instead of
// IdentifiedOntologyError) → unaffected here, pinned by the repos test of the
// accessor; drop identify's symlink refusal → "linked" is active → red.
func TestGetRepos_OntologyErrorOnTheWire(t *testing.T) {
	const agentBranch = "machine/test"
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{Cfg: config.Config{Home: home}, AgentBranch: agentBranch})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	r := (&Server{Manager: m}).NewAPIRouter()
	for _, n := range []string{"linked", "missing", "garbled", "empty", "fine"} {
		createViaAPI(t, r, n)
	}

	ctx := context.Background()
	breakIt := func(name string, f func(svc *store.Service) error) {
		t.Helper()
		ri := m.Get(name)
		if ri == nil {
			t.Fatalf("repo %s not mounted", name)
		}
		var ferr error
		if err := ri.WithRead(func(svc *store.Service) { ferr = f(svc) }); err != nil {
			t.Fatalf("acquire %s: %v", name, err)
		}
		if ferr != nil {
			t.Fatalf("break %s: %v", name, ferr)
		}
	}
	// The link TEXT is itself a parseable ontology, so a reader that followed
	// or parsed it would mount the repo with no error.
	const linkText = "id: crafted\nname: Crafted\ntopics:\n  notes:\n    description: N\n"
	breakIt("linked", func(svc *store.Service) error {
		_, err := svc.RawSymlinkForTest(ctx, agentBranch, fact.OntologyFile, linkText, "symlink the ontology")
		return err
	})
	breakIt("missing", func(svc *store.Service) error {
		_, err := svc.Facts().DeleteFact(ctx, agentBranch, fact.OntologyFile, "remove the ontology")
		return err
	})
	breakIt("garbled", func(svc *store.Service) error {
		// Valid YAML that is not an ontology: id is required.
		_, err := svc.RawWriteForTest(ctx, agentBranch, fact.OntologyFile, "topics:\n  x:\n    description: y\n", "garble the ontology")
		return err
	})
	breakIt("empty", func(svc *store.Service) error {
		_, err := svc.RawWriteForTest(ctx, agentBranch, fact.OntologyFile, "", "empty the ontology")
		return err
	})
	if err := m.Close(); err != nil {
		t.Fatalf("close manager: %v", err)
	}

	r = (&Server{Manager: newRealManagerInHome(t, home)}).NewAPIRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /repos: status %d body %s", rec.Code, rec.Body.String())
	}
	list := rec.Body.Bytes()

	// Symlink: refused at Identify, then archived automatically at boot
	// (option B, user ruling 2026-10-09) — so it is NOT a GET /repos row; it is
	// in GET /archived with a system reason naming the symlink, and its old
	// URL answers 404 with that reason.
	if repoRowOK(list, "linked") {
		t.Fatalf("linked: a symlinked ontology is archived at boot, but GET /repos still lists it: %s", list)
	}
	arch := httptest.NewRecorder()
	r.ServeHTTP(arch, httptest.NewRequest(http.MethodGet, "/archived", nil))
	var archived struct {
		Embedded struct {
			Archived []struct {
				Name   string `json:"name"`
				Reason *struct {
					Source, Reason, Condition, Summary string
				} `json:"reason"`
			} `json:"archived"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(arch.Body.Bytes(), &archived); err != nil {
		t.Fatalf("GET /archived: %v: %s", err, arch.Body.String())
	}
	if len(archived.Embedded.Archived) != 1 || archived.Embedded.Archived[0].Name != "linked" {
		t.Fatalf("GET /archived: want exactly [linked], got %s", arch.Body.String())
	}
	why := archived.Embedded.Archived[0].Reason
	if why == nil || why.Source != "system" ||
		!strings.Contains(why.Reason, fact.OntologyFile+" is a symlink, and knomit does not follow symlinks") ||
		why.Condition != "unopenable" {
		t.Fatalf("linked: archive reason = %+v, want system / the named symlink / unopenable", why)
	}
	one := httptest.NewRecorder()
	r.ServeHTTP(one, httptest.NewRequest(http.MethodGet, "/repos/linked", nil))
	if one.Code != http.StatusNotFound || !strings.Contains(one.Body.String(), "Archived automatically") ||
		!strings.Contains(one.Body.String(), "is a symlink") {
		t.Fatalf("GET /repos/linked: status %d body %s, want 404 carrying the archive reason", one.Code, one.Body.String())
	}

	// Missing, garbled, empty: mounted, and the reason is on the row and on
	// the single GET, identically.
	for name, want := range map[string]string{
		"missing": "no ontology at " + fact.OntologyFile,
		"garbled": fact.OntologyFile + " does not parse",
		"empty":   fact.OntologyFile + " on " + agentBranch + " is empty",
	} {
		row := repoRow(t, list, name)
		if row["state"] != "active" {
			t.Fatalf("%s: state = %v, want active (readable): %v", name, row["state"], row)
		}
		got, _ := row["ontology_error"].(string)
		if !strings.Contains(got, want) {
			t.Fatalf("%s: ontology_error = %q, want it to contain %q", name, got, want)
		}
		single := getRepoBody(t, r, name)
		if single["ontology_error"] != got {
			t.Fatalf("%s: single GET ontology_error = %v, list says %q", name, single["ontology_error"], got)
		}
	}

	// Regular: no field on either.
	fine := repoRow(t, list, "fine")
	if _, ok := fine["ontology_error"]; ok || fine["state"] != "active" {
		t.Fatalf("fine: want an active row with no ontology_error: %v", fine)
	}
	if v, ok := getRepoBody(t, r, "fine")["ontology_error"]; ok {
		t.Fatalf("fine: single GET carries ontology_error %v", v)
	}
}

// Restoring an archived repo whose ontology is a symlink: Identify refuses
// it, the restore is undone, and the answer is a 422 naming the file — not
// the default arm's 500. The repo stays archived, so it can still be purged.
// Sabotage: drop the ErrSymlinkNotFollowed arm of archiveErrStatus → 500 →
// red; drop identify's symlink refusal → the restore succeeds (200) → red.
func TestRestore_SymlinkedOntologyIs422AndStaysArchived(t *testing.T) {
	const agentBranch = "machine/test"
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "linked")
	ri := s.Manager.Get("linked")
	var serr error
	if err := ri.WithRead(func(svc *store.Service) {
		_, serr = svc.RawSymlinkForTest(context.Background(), agentBranch, fact.OntologyFile,
			"id: crafted\nname: C\ntopics:\n  n:\n    description: N\n", "symlink the ontology")
	}); err != nil || serr != nil {
		t.Fatalf("symlink the ontology: %v / %v", err, serr)
	}

	drec := httptest.NewRecorder()
	r.ServeHTTP(drec, fromLoopback(httptest.NewRequest(http.MethodDelete, "/repos/linked", nil)))
	if drec.Code != http.StatusOK {
		t.Fatalf("archive: status %d body %s", drec.Code, drec.Body.String())
	}
	var archived struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(drec.Body.Bytes(), &archived); err != nil {
		t.Fatalf("decode archive: %v", err)
	}

	rrec := httptest.NewRecorder()
	r.ServeHTTP(rrec, fromLoopback(newJSONRequest(http.MethodPost, "/archived/"+archived.ID+"/restore", strings.NewReader(`{}`))))
	if rrec.Code != http.StatusUnprocessableEntity || !strings.Contains(rrec.Body.String(), fact.OntologyFile+" is a symlink") {
		t.Fatalf("restore: status %d body %s, want 422 naming the symlink", rrec.Code, rrec.Body.String())
	}
	if s.Manager.Get("linked") != nil {
		t.Fatal("a refused restore must not mount the repo")
	}
	grec := httptest.NewRecorder()
	r.ServeHTTP(grec, httptest.NewRequest(http.MethodGet, "/archived", nil))
	if !strings.Contains(grec.Body.String(), archived.ID) {
		t.Fatalf("refused restore lost the archive row: %s", grec.Body.String())
	}
	prec := httptest.NewRecorder()
	r.ServeHTTP(prec, fromLoopback(httptest.NewRequest(http.MethodDelete, "/archived/"+archived.ID, nil)))
	if prec.Code != http.StatusNoContent {
		t.Fatalf("purge after the refused restore: status %d body %s, want 204", prec.Code, prec.Body.String())
	}
}

func getRepoBody(t *testing.T, r http.Handler, name string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/"+name, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /repos/%s: status %d body %s", name, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET /repos/%s: bad body %q: %v", name, rec.Body.String(), err)
	}
	return body
}

// repoRowOK reports whether the GET /repos body has a row named name.
func repoRowOK(raw []byte, name string) bool {
	var body struct {
		Embedded struct {
			Repos []map[string]any `json:"repos"`
		} `json:"_embedded"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	for _, row := range body.Embedded.Repos {
		if row["name"] == name {
			return true
		}
	}
	return false
}
