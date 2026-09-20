package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// newExperimentRESTManager builds a manager holding one repo with a REAL
// store, because write eligibility for an exp/* branch reads the experiments
// table — a storeless test instance would refuse every experiment for the
// wrong reason and the assertions below would be vacuous.
func newExperimentRESTManager(t *testing.T, name string) (*repos.Manager, *store.Service) {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if err := svc.InitRepo(map[string]string{}, "agent/test"); err != nil {
		t.Fatalf("init repo: %v", err)
	}
	m := repos.New(context.Background(), repos.Deps{})
	m.Set(name, repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: name, AgentBranch: "agent/test", Svc: svc, OntologyRoot: "know",
	}))
	return m, svc
}

func openRESTExperiment(t *testing.T, svc *store.Service, name string) {
	t.Helper()
	if _, err := svc.Experiments().OpenExperiment(context.Background(), name, "", "agent/test"); err != nil {
		t.Fatalf("open experiment: %v", err)
	}
}

// TestHandleFactCreate_AcceptsOwnExperimentBranch: the REST fact handlers
// needed no change for experiments — they already gate on
// refuseUnwritableBranch, which consults RepoInstance.WritableBranch, so
// widening the classification reaches them for free. That is a CLAIM, and
// this test is what makes it checkable rather than asserted.
func TestHandleFactCreate_AcceptsOwnExperimentBranch(t *testing.T) {
	m, svc := newExperimentRESTManager(t, "alpha")
	openRESTExperiment(t, svc, "rest-write")

	s := &Server{
		Manager:      m,
		OntologyRoot: "know",
		providers:    storeProviders{factWriter: stubFactWriterForCreate{}},
	}
	r := s.NewAPIRouter()

	body := `{"title":"My Fact","body":"some body","type":"observation","domain":["ai"],"confidence":0.9,"sources":1}`
	rec := httptest.NewRecorder()
	// "exp:rest-write" is the URL spelling of "exp/rest-write" — the router
	// substitutes ":" for "/" in a branch segment.
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/exp:rest-write/facts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201 on an own experiment, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleFactCreate_RefusesForeignExperimentBranch is the other half: the
// gate still bites on an experiment forked from something that is not this
// instance's agent branch, and on an exp/* ref with no record at all.
func TestHandleFactCreate_RefusesForeignExperimentBranch(t *testing.T) {
	m, svc := newExperimentRESTManager(t, "alpha")
	ctx := context.Background()
	if err := svc.Branches().CreateBranch(ctx, "agent/elsewhere", "agent/test"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if _, err := svc.Experiments().OpenExperiment(ctx, "theirs", "", "agent/elsewhere"); err != nil {
		t.Fatalf("open experiment: %v", err)
	}
	// A bare ref in the namespace with no experiments row behind it.
	if err := svc.Branches().CreateBranch(ctx, "exp/unrecorded", "agent/test"); err != nil {
		t.Fatalf("create branch: %v", err)
	}

	s := &Server{
		Manager:      m,
		OntologyRoot: "know",
		providers:    storeProviders{factWriter: stubFactWriterForCreate{}},
	}
	r := s.NewAPIRouter()

	for _, branch := range []string{"exp:theirs", "exp:unrecorded", "main"} {
		body := `{"title":"My Fact","body":"some body","type":"observation","domain":["ai"],"confidence":0.9,"sources":1}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/"+branch+"/facts", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("branch %q: status got %d, want 403, body=%s", branch, rec.Code, rec.Body.String())
		}
	}
}
