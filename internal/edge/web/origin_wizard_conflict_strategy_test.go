package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/engine/store"
)

// originFactDoc renders a minimal valid fact file whose body carries the
// marker the assertions look for.
func originFactDoc(title, body string) string {
	return "---\ntype: observation\nconfidence: 0.9\n---\n# " + title + "\n\n" + body + "\n"
}

// TestConnect_ConflictStrategyReachesTheReplay drives the whole HTTP connect
// wizard (create session, /test, /apply, /commit) against a disjoint remote
// that shares one path with the local repo, once per conflict_strategy, and
// reads the outcome back through GET /facts. It ports the intent of the e2e
// specs 'migrate local facts to a remote knomit instance' and 'remote_wins
// strategy preserves remote content on shared paths', which could never run
// because the e2e harness cannot seed a remote's published main.
//
// Every other wizard test applies local_wins, so without the remote_wins row
// nothing under internal/edge would notice /apply ignoring the request's
// strategy. The local_wins row is the contrast: the SAME fixture must yield the
// LOCAL body on the shared path, which proves the parameter decides it.
//
// It also ports the e2e spec's post-commit checks: the session is gone (404)
// and the origin config is persisted with the remote's URL and branch.
//
// SABOTAGE: force strategy = merge.StrategyLocalWins in handleApply
// (internal/edge/web/api/origins/handlers.go, the `Strategy: strategy` line of
// the ReplayConfig) regardless of req.ConflictStrategy -> the remote_wins row
// sees the local body -> red.
func TestConnect_ConflictStrategyReachesTheReplay(t *testing.T) {
	const (
		sharedPath     = "kb/shared/protocol.md"
		localOnlyPath  = "kb/local-only/data.md"
		remoteOnlyPath = "kb/remote-only/data.md"
		remoteMarker   = "Remote version of the protocol."
		localMarker    = "Local version of the protocol."
	)
	for _, tc := range []struct {
		strategy     string
		wantShared   string
		absentShared string
	}{
		{"remote_wins", remoteMarker, localMarker},
		{"local_wins", localMarker, remoteMarker},
	} {
		t.Run(tc.strategy, func(t *testing.T) {
			s, m, remotesRoot := newIdentitySessionServer(t)
			ri := createLocalRepo(t, m, "core")

			if err := ri.WithRead(func(svc *store.Service) {
				for path, doc := range map[string]string{
					sharedPath:    originFactDoc("Local Protocol", localMarker),
					localOnlyPath: originFactDoc("Local Data", "Only on local."),
				} {
					if _, err := svc.Facts().WriteFact(context.Background(), "agent/test", path, doc, "learn: "+path, "learn"); err != nil {
						t.Errorf("local write %s: %v", path, err)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}

			url := seedKnomitRemoteFilesForTest(t, filepath.Join(remotesRoot, "upstream.git"), "upstream", map[string]string{
				sharedPath:     originFactDoc("Remote Protocol", remoteMarker),
				remoteOnlyPath: originFactDoc("Remote Data", "Only on remote."),
			})

			sessID := startOriginSession(t, s, "core", url)
			base := "/repos/core/origin-sessions/" + sessID
			if body := sseCall(t, s, http.MethodGet, base+"/test", ""); !strings.Contains(body, `"history":"disjoint"`) {
				t.Fatalf("fixture: want a disjoint remote; body=%s", body)
			}
			if body := sseCall(t, s, http.MethodPost, base+"/apply", `{"conflict_strategy":"`+tc.strategy+`"}`); strings.Contains(body, `"phase":"error"`) || !strings.Contains(body, `"phase":"done"`) {
				t.Fatalf("apply: %s", body)
			}
			if body := sseCall(t, s, http.MethodPost, base+"/commit", ""); strings.Contains(body, `"phase":"error"`) || !strings.Contains(body, `"phase":"done"`) {
				t.Fatalf("commit: %s", body)
			}

			// The commit finalizes the session: it must no longer resolve.
			{
				rec := httptest.NewRecorder()
				s.NewAPIRouter().ServeHTTP(rec, fromLoopback(httptest.NewRequest(http.MethodGet, base, nil)))
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s: session after commit: status %d, want 404", tc.strategy, rec.Code)
				}
			}
			// The origin config is persisted: the remote's URL and the branch
			// the session tracked (the seeded remote's default, main).
			{
				rec := httptest.NewRecorder()
				s.NewAPIRouter().ServeHTTP(rec, fromLoopback(httptest.NewRequest(http.MethodGet, "/repos/core/origin", nil)))
				var origin struct {
					URL    string `json:"url"`
					Branch string `json:"branch"`
				}
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &origin) != nil {
					t.Fatalf("%s: GET origin: status %d body %s", tc.strategy, rec.Code, rec.Body.String())
				}
				if origin.URL != url || origin.Branch != "main" {
					t.Errorf("%s: origin = {url %q, branch %q}, want {%q, main}", tc.strategy, origin.URL, origin.Branch, url)
				}
			}

			factBody := func(path string) (int, string) {
				rec := httptest.NewRecorder()
				req := fromLoopback(httptest.NewRequest(http.MethodGet,
					"/repos/core/branches/"+urlBranch("agent/test")+"/facts/"+path, nil))
				s.NewAPIRouter().ServeHTTP(rec, req)
				var out struct {
					Body string `json:"body"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &out)
				return rec.Code, out.Body
			}

			code, got := factBody(sharedPath)
			if code != http.StatusOK {
				t.Fatalf("shared fact: status %d", code)
			}
			if !strings.Contains(got, tc.wantShared) || strings.Contains(got, tc.absentShared) {
				t.Errorf("%s: shared path body = %q, want it to hold %q and not %q", tc.strategy, got, tc.wantShared, tc.absentShared)
			}
			if code, got := factBody(localOnlyPath); code != http.StatusOK || !strings.Contains(got, "Only on local.") {
				t.Errorf("%s: local-only fact lost: status %d body %q", tc.strategy, code, got)
			}
			if code, got := factBody(remoteOnlyPath); code != http.StatusOK || !strings.Contains(got, "Only on remote.") {
				t.Errorf("%s: remote-only fact missing: status %d body %q", tc.strategy, code, got)
			}
		})
	}
}
