package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"knomit/internal/repos"
)

type wireReason struct {
	Source    string `json:"source"`
	Reason    string `json:"reason"`
	Note      string `json:"note"`
	Condition string `json:"condition"`
	Summary   string `json:"summary"`
}

type wireArchived struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Reason *wireReason `json:"reason"`
	Links  map[string]struct {
		Href string `json:"href"`
	} `json:"_links"`
}

func archiveViaAPI(t *testing.T, r http.Handler, name, body string) (int, wireArchived, string) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodDelete, "/repos/"+name, nil)
	} else {
		req = newJSONRequest(http.MethodDelete, "/repos/"+name, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, fromLoopback(req))
	var got wireArchived
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode archive body: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, got, rec.Body.String()
}

// DELETE /repos/{repo} takes an optional {"note"}; the reason comes back on
// the archive response, on GET /archived, and on GET /archived/{id} (the
// item's own self link, which had no route before). Without a body the
// reason is a user archive with no note. An over-long note is a 400 and
// archives nothing.
//
// Sabotage: drop the GET /archived/{id} route → 405/404 → red; drop the
// reason from archivedViewOf → reason null → red; drop the note cap → 200 →
// red.
func TestArchive_ReasonOnTheWire(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "work")
	createViaAPI(t, r, "bare")

	code, _, body := archiveViaAPI(t, r, "work", `{"note":"`+strings.Repeat("x", 501)+`"}`)
	if code != http.StatusBadRequest || s.Manager.Get("work") == nil {
		t.Fatalf("long note: status %d body %s, want 400 and the repo still active", code, body)
	}

	code, got, body := archiveViaAPI(t, r, "work", `{"note":"  moved to the team KB  "}`)
	if code != http.StatusOK {
		t.Fatalf("archive: status %d body %s", code, body)
	}
	want := wireReason{Source: "user", Note: "moved to the team KB", Summary: "Archived by the user: moved to the team KB"}
	if got.Reason == nil || *got.Reason != want {
		t.Fatalf("archive response reason = %+v, want %+v", got.Reason, want)
	}

	code, bare, body := archiveViaAPI(t, r, "bare", "")
	if code != http.StatusOK || bare.Reason == nil || bare.Reason.Summary != "Archived by the user" || bare.Reason.Source != "user" {
		t.Fatalf("bodyless archive: status %d body %s", code, body)
	}

	item := httptest.NewRecorder()
	r.ServeHTTP(item, httptest.NewRequest(http.MethodGet, got.Links["self"].Href[len("/api/v1"):], nil))
	if item.Code != http.StatusOK {
		t.Fatalf("GET self %s: status %d body %s", got.Links["self"].Href, item.Code, item.Body.String())
	}
	var one wireArchived
	if err := json.Unmarshal(item.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if one.ID != got.ID || one.Reason == nil || *one.Reason != want {
		t.Fatalf("GET /archived/{id} = %+v, want the same reason", one)
	}

	list := httptest.NewRecorder()
	r.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/archived", nil))
	if !strings.Contains(list.Body.String(), `"summary":"Archived by the user: moved to the team KB"`) {
		t.Fatalf("GET /archived does not carry the reason: %s", list.Body.String())
	}

	nf := httptest.NewRecorder()
	r.ServeHTTP(nf, httptest.NewRequest(http.MethodGet, "/archived/nope", nil))
	if nf.Code != http.StatusNotFound {
		t.Fatalf("GET /archived/nope: status %d, want 404", nf.Code)
	}

	// The archived name's URL now says why it is gone.
	gone := httptest.NewRecorder()
	r.ServeHTTP(gone, httptest.NewRequest(http.MethodGet, "/repos/work", nil))
	if gone.Code != http.StatusNotFound || !strings.Contains(gone.Body.String(), "Archived by the user: moved to the team KB") {
		t.Fatalf("GET /repos/work: status %d body %s, want 404 carrying the summary", gone.Code, gone.Body.String())
	}
}

// An UNAVAILABLE repo (here: missing) is archived through the same DELETE —
// it used to 404 because archive resolved only mounted repos — and its state
// is recorded as the reason's condition. A legacy archive (no reason row)
// reads reason: null.
func TestArchive_UnavailableRepoAndLegacyOnTheWire(t *testing.T) {
	home := t.TempDir()
	m := newRealManagerInHome(t, home)
	r := (&Server{Manager: m}).NewAPIRouter()
	createViaAPI(t, r, "broken")
	createViaAPI(t, r, "old")
	uid := m.Get("broken").UID()
	oldUID := m.Get("old").UID()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.RepoPath(uid)); err != nil {
		t.Fatal(err)
	}

	m2 := newRealManagerInHome(t, home)
	r = (&Server{Manager: m2}).NewAPIRouter()
	code, got, body := archiveViaAPI(t, r, "broken", `{"note":"lost the disk"}`)
	if code != http.StatusOK {
		t.Fatalf("archive an unavailable repo: status %d body %s, want 200", code, body)
	}
	if got.Reason == nil || got.Reason.Condition != "missing: database file not found" || got.Reason.Note != "lost the disk" {
		t.Fatalf("reason = %+v, want the missing condition and the note", got.Reason)
	}
	if len(m2.Unavailable()) != 0 {
		t.Fatalf("still listed unavailable: %+v", m2.Unavailable())
	}

	// Legacy: a bare state flip, as every build before reasons archived.
	m2.Remove("old")
	if err := m2.Repos().SetState(oldUID, repos.StateArchived, 1); err != nil {
		t.Fatal(err)
	}
	item := httptest.NewRecorder()
	r.ServeHTTP(item, httptest.NewRequest(http.MethodGet, "/archived/"+oldUID, nil))
	if item.Code != http.StatusOK || !strings.Contains(item.Body.String(), `"reason":null`) {
		t.Fatalf("legacy item: status %d body %s, want reason null", item.Code, item.Body.String())
	}
	plain := httptest.NewRecorder()
	r.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/repos/old", nil))
	if !strings.Contains(plain.Body.String(), `"detail":"no repo named \"old\""`) {
		t.Fatalf("a legacy archive keeps the plain 404: %s", plain.Body.String())
	}
}
