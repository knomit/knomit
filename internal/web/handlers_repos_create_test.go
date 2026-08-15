package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
)

func newRealManager(t *testing.T) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "machine/test",
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// newRealManagerWithLocalOriginRoot is newRealManager with LocalOriginRoot set
// to root, so a file:// origin under it clears the local-origin policy gate.
// newRealManager itself must keep no root configured — other tests depend on
// filesystem origins being disabled by default — so this is a separate helper
// rather than a parameter added to it.
func newRealManagerWithLocalOriginRoot(t *testing.T, root string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), LocalOriginRoot: root},
		AgentBranch: "machine/test",
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestPostRepos_StreamsNDJSONAndCreates(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"work","mode":"preset","ontology_preset":"default"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/x-ndjson") {
		t.Fatalf("content-type = %q", ct)
	}
	var lastType string
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad ndjson line %q: %v", line, err)
		}
		lastType, _ = m["type"].(string)
	}
	if lastType != "done" {
		t.Fatalf("last type = %q, want done", lastType)
	}
	if s.Manager.Get("work") == nil {
		t.Fatal("repo not registered")
	}
}

func TestPostRepos_ConflictOnExistingName(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "work")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"work","mode":"preset","ontology_preset":"default"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestPostRepos_BadNameReturns400(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"Bad Name","mode":"preset"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// A seed against a remote that already has refs must be refused BEFORE the
// NDJSON stream opens, as a real 409 — the status the OpenAPI documents for
// this case. Until CreatePreflight probed, ErrRemoteNotEmpty could only be
// produced inside Create, i.e. after w.WriteHeader(200), so createErrStatus's
// ErrRemoteNotEmpty case was unreachable and the documented 409 was a lie.
func TestPostRepos_SeedNonEmptyRemoteIs409NotAStreamedError(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	if err := exec.Command("git", "init", "--bare", remote).Run(); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init"}, {"remote", "add", "origin", remote}, {"push", "origin", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	body := `{"name":"kb","mode":"seed","ontology_preset":"default","origin":{"url":"file://` + remote + `"}}`
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos", strings.NewReader(body)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "ndjson") {
		t.Fatalf("a pre-stream refusal must not open the NDJSON stream; content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "not empty") {
		t.Fatalf("body does not name the cause: %s", rec.Body.String())
	}
}

// MaxOntologyBytes claims to cap "an ontology accepted by :validate and by
// create". The create path decoded its body with no MaxBytesReader at all, so
// ontology_yaml was unbounded there and the comment asserted a guard that did
// not exist.
func TestPostRepos_OversizeBodyIs413(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	huge := strings.Repeat("x", MaxOntologyBytes+1)
	body := `{"name":"kb","mode":"custom","ontology_yaml":"` + huge + `"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos", strings.NewReader(body)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if s.Manager.Get("kb") != nil {
		t.Fatal("an oversize create must not register a repo")
	}
}

// The cap must not be so tight that an ordinary create trips it: a body just
// under the limit still gets its normal answer (here, a 400 for a body that is
// valid JSON but not a valid ontology) rather than a 413.
func TestPostRepos_BodyUnderTheCapIsNotRejectedAsOversize(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	// Comment padding keeps the YAML valid while making the body large.
	yaml := "id: x\\nname: X\\ntopics:\\n  alpha:\\n    description: d\\n" +
		strings.Repeat("# pad\\n", 1000)
	body := `{"name":"kb","mode":"custom","ontology_yaml":"` + yaml + `"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos", strings.NewReader(body)))

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("a body well under the cap was rejected as oversize: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
