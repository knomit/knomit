package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// initializedResponse mirrors what the endpoint serialises, so these tests
// assert on the WIRE shape the web client actually consumes rather than on the
// Go struct. The pointer is deliberate: it is how "the field was absent" is
// told apart from "the field was the empty string", and the third state is
// carried by exactly that absence.
type initializedResponse struct {
	Initialized *string `json:"initialized"`
	Branch      string  `json:"branch"`
	Detail      string  `json:"detail"`
}

func postProbeInitialized(t *testing.T, r http.Handler, body string) (*httptest.ResponseRecorder, initializedResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos:probe-initialized", strings.NewReader(body)))
	var got initializedResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
		}
	}
	return rec, got
}

// A remote that IS a knowledge base answers "yes" and names the branch it
// looked at.
func TestProbeInitialized_HTTP_ReportsYes(t *testing.T) {
	root := t.TempDir()
	url := seedKnomitRemoteForTest(t, filepath.Join(root, "remote.git"), "seed")

	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	rec, got := postProbeInitialized(t, s.NewAPIRouter(),
		`{"url":"`+url+`","branch":"main"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got.Initialized == nil || *got.Initialized != "yes" {
		t.Fatalf("initialized = %v, want \"yes\"; body=%s", got.Initialized, rec.Body.String())
	}
	if got.Branch != "main" {
		t.Fatalf("branch = %q, want \"main\"", got.Branch)
	}
}

// A plain git repository — a branch, commits, no ontology — answers "no". This
// is the case the whole "initialize" mode exists for, and the case that used to
// be routed to clone and silently given the default ontology.
func TestProbeInitialized_HTTP_ReportsNo(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	url := seedPlainRemoteForTest(t, bare)

	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	rec, got := postProbeInitialized(t, s.NewAPIRouter(),
		`{"url":"`+url+`","branch":"main"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got.Initialized == nil || *got.Initialized != "no" {
		t.Fatalf("initialized = %v, want \"no\"; body=%s", got.Initialized, rec.Body.String())
	}
}

// THE THIRD STATE, on the wire. A check that did not complete comes back as a
// 200 with the `initialized` field ABSENT — not as an error status, and not as
// `"no"`. A client that reads it as either answer destroys data: "yes" discards
// the ontology the user chose, "no" writes one over a knowledge base that
// already had its own, and the ontology is immutable after creation.
func TestProbeInitialized_HTTP_UnknownIs200WithTheFieldAbsent(t *testing.T) {
	root := t.TempDir()
	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	rec, got := postProbeInitialized(t, s.NewAPIRouter(),
		`{"url":"file://`+filepath.Join(root, "does-not-exist.git")+`","branch":"main"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a check that failed is a RESULT; body=%s", rec.Code, rec.Body.String())
	}
	if got.Initialized != nil {
		t.Fatalf("initialized = %q, want the field to be ABSENT for an unestablished check", *got.Initialized)
	}
	if strings.Contains(rec.Body.String(), `"initialized"`) {
		t.Fatalf("the unknown state must OMIT the field, not send an empty one: %s", rec.Body.String())
	}
	if got.Detail == "" {
		t.Fatal("an unknown must carry the reason it is unknown")
	}
}

func TestProbeInitialized_HTTP_MissingURLIs400(t *testing.T) {
	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, t.TempDir())}
	rec, _ := postProbeInitialized(t, s.NewAPIRouter(), `{"branch":"main"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestProbeInitialized_HTTP_MalformedBodyIs400(t *testing.T) {
	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, t.TempDir())}
	rec, _ := postProbeInitialized(t, s.NewAPIRouter(), `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// seedPlainRemoteForTest builds a bare repo with one commit on main and NO
// ontology: an ordinary git repository that is not a knowledge base. The
// counterpart of seedKnomitRemoteForTest, which writes one.
func seedPlainRemoteForTest(t *testing.T, bare string) string {
	t.Helper()
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir bare: %v", err)
	}
	runGitForTest(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGitForTest(t, "", "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# not a kb\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGitForTest(t, work, "add", "-A")
	runGitForTest(t, work, "commit", "-m", "readme")
	runGitForTest(t, work, "push", "origin", "main")
	runGitForTest(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return "file://" + bare
}

// Guard on the premise the two fixtures above encode: OntologyPath is what
// separates them, and fact.DefaultOntology must still serialize into it. If
// this ever fails, every "yes" assertion in this file is testing nothing.
func TestSeedFixtures_DifferOnlyByTheOntology(t *testing.T) {
	if _, err := fact.DefaultOntology().Serialize(); err != nil {
		t.Fatalf("the fixtures cannot write an ontology: %v", err)
	}
	if repos.OntologyPath == "" {
		t.Fatal("OntologyPath is empty — the fixtures write nothing distinguishing")
	}
}
