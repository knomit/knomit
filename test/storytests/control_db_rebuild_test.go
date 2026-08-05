// Acceptance gate for moving origin credentials into the control plane: a home
// holding ONLY control.db (plus the agent key that decrypts it) must rebuild
// every repo it claims exists — private, credentialed origin included — with no
// re-authentication.
package storytests

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web"
	"knomit/test/testenv"
)

// The private origin's credentials. The operator supplies them as basic auth
// ("user:password" in one token field, which is how control.db stores every
// credential), and the test git server demands exactly those bytes — so the
// clone genuinely cannot succeed without the recovered credential.
const (
	privateUser = "git"
	privatePass = "s3cret"
)

// controlAppAgentBranch is the agent branch every repo in the test app gets.
// Its URL form replaces "/" with ":" (hal.EncodeBranch).
const (
	controlAppAgentBranch    = "agent/test"
	controlAppAgentBranchURL = "agent:test"
)

// controlApp is a knomit instance: one home, one repos.Manager, and the real
// HTTP API router mounted over it. Unlike a Storyboard RepoHandle (one repo per
// home, no API), it holds SEVERAL repos in ONE home and can be stopped and
// started again against the same directory — which is exactly the shape this
// acceptance test needs, since the property under test is about what a home
// can rebuild from control.db across a restart.
//
// The API router is here on purpose: "the repo appears/does not appear in the
// API" is the observable the boot path's own error messages promise, and
// GET /api/v1/repos serves it straight from the manager's live instances.
type controlApp struct {
	t        *testing.T
	home     string
	embedder store.BatchEmbedder
	m        *repos.Manager
	api      http.Handler
}

// newControlApp writes the agent key into a fresh home and boots the app.
//
// The key lives at <home>/id_ed25519 — the path repos.Deps.KeyPath falls back
// to — and is NOT under repos/, so it survives the database wipe below. That is
// the honest claim of this plan: control.db plus the agent key is sufficient,
// control.db alone is not (the credential is encrypted with a key derived from
// this file).
func newControlApp(t *testing.T, home string) *controlApp {
	t.Helper()
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, "id_ed25519"),
		[]byte("acceptance-agent-key"), 0o600))
	a := &controlApp{t: t, home: home, embedder: &testenv.DeterministicEmbedder{}}
	t.Cleanup(a.Stop)
	a.Start()
	return a
}

// Start boots a manager over the home and mounts the API router on it. Calling
// it after Stop is a re-boot of the same directory — the process restart this
// test is built around.
func (a *controlApp) Start() {
	a.t.Helper()
	cfg := config.Config{Home: a.home}
	// Loopback smart-HTTP clones finish in milliseconds; this only stops a
	// pathological hang from burning the whole test budget.
	cfg.Git.NetworkTimeout = 30 * time.Second
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   cfg,
		AgentBranch:           controlAppAgentBranch,
		Embedder:              a.embedder,
		KeyPath:               filepath.Join(a.home, "id_ed25519"),
		DisableBackgroundSync: true,
	})
	require.NoError(a.t, m.Start(), "app boot")
	a.m = m
	srv := &web.Server{Manager: m, AgentBranch: controlAppAgentBranch, OntologyRoot: "kb"}
	a.api = srv.NewAPIRouter()
}

// Stop shuts the app down. Idempotent, so an explicit Stop mid-test and the
// registered cleanup can both run.
func (a *controlApp) Stop() {
	if a.m == nil {
		return
	}
	_ = a.m.Close()
	a.m = nil
	a.api = nil
}

// call issues one API request and returns the recorder.
func (a *controlApp) call(method, path, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	a.api.ServeHTTP(rec, r)
	return rec
}

// CreateFromOrigin creates a repo through POST /api/v1/repos in clone mode,
// carrying the credential in the request body exactly as an operator would.
// Returns the error the NDJSON stream reported, or nil.
//
// POST /repos answers 200 and then streams progress, so a failed create is a
// terminal {"type":"error"} line, NOT an HTTP status — hence the scan.
func (a *controlApp) CreateFromOrigin(name, url, authMethod, authToken string) error {
	a.t.Helper()
	body, err := json.Marshal(map[string]any{
		"name": name, "mode": "clone",
		"origin": map[string]string{
			"url": url, "auth_method": authMethod, "auth_token": authToken,
		},
	})
	require.NoError(a.t, err)
	rec := a.call(http.MethodPost, "/repos", string(body))
	return createStreamError(a.t, name, rec)
}

// CreateLocal creates an origin-less repo (preset ontology) — the genuinely
// unrecoverable case, since its only copy of its history is its own database.
func (a *controlApp) CreateLocal(name string) {
	a.t.Helper()
	rec := a.call(http.MethodPost, "/repos",
		`{"name":"`+name+`","mode":"preset","ontology_preset":"default"}`)
	require.NoError(a.t, createStreamError(a.t, name, rec))
}

// createStreamError decodes a create's NDJSON stream and returns the terminal
// error line as an error, or nil when the stream ended in "done".
func createStreamError(t *testing.T, name string, rec *httptest.ResponseRecorder) error {
	t.Helper()
	if rec.Code != http.StatusOK {
		return fmt.Errorf("create %s: HTTP %d: %s", name, rec.Code, rec.Body.String())
	}
	var last string
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev struct {
			Type   string `json:"type"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return fmt.Errorf("create %s: bad ndjson line %q: %w", name, line, err)
		}
		last = ev.Type
		if ev.Type == "error" {
			return fmt.Errorf("create %s: %s: %s", name, ev.Title, ev.Detail)
		}
	}
	if last != "done" {
		return fmt.Errorf("create %s: stream ended in %q, not \"done\": %s",
			name, last, rec.Body.String())
	}
	return nil
}

// WriteFact creates one fact on a repo's agent branch through the API. Used to
// give the origin-less repo content of its OWN — a preset create seeds an
// ontology, not facts, and the point of that repo is that its only copy of its
// history lives in its database.
func (a *controlApp) WriteFact(repo, title string) {
	a.t.Helper()
	body, err := json.Marshal(map[string]any{
		"title": title, "body": "Recorded only here; this origin-less repo has no remote copy.",
		"domain": []string{"gotchas", "local"},
	})
	require.NoError(a.t, err)
	rec := a.call(http.MethodPost,
		"/repos/"+repo+"/branches/"+controlAppAgentBranchURL+"/facts", string(body))
	require.Equal(a.t, http.StatusCreated, rec.Code,
		"POST fact to %s: %s", repo, rec.Body.String())
}

// APIRepoNames returns the repo names GET /api/v1/repos serves. This is the
// "appears in the API" observable: the handler lists the manager's live
// instances, so a repo the boot could not open or rebuild is absent here.
func (a *controlApp) APIRepoNames() []string {
	a.t.Helper()
	rec := a.call(http.MethodGet, "/repos", "")
	require.Equal(a.t, http.StatusOK, rec.Code, "GET /repos: %s", rec.Body.String())
	var body struct {
		Embedded struct {
			Repos []struct {
				Name string `json:"name"`
			} `json:"repos"`
		} `json:"_embedded"`
	}
	require.NoError(a.t, json.Unmarshal(rec.Body.Bytes(), &body))
	names := make([]string, 0, len(body.Embedded.Repos))
	for _, r := range body.Embedded.Repos {
		names = append(names, r.Name)
	}
	return names
}

// FactPaths returns the fact paths the API serves on a repo's agent branch.
// Non-empty means the repo is not merely registered but actually readable —
// its git history and its index both survived (or were rebuilt).
func (a *controlApp) FactPaths(repo string) []string {
	a.t.Helper()
	rec := a.call(http.MethodGet,
		"/repos/"+repo+"/branches/"+controlAppAgentBranchURL+"/facts?limit=100", "")
	require.Equal(a.t, http.StatusOK, rec.Code,
		"GET facts for %s: %s", repo, rec.Body.String())
	var body struct {
		Embedded struct {
			Facts []struct {
				Path string `json:"path"`
			} `json:"facts"`
		} `json:"_embedded"`
	}
	require.NoError(a.t, json.Unmarshal(rec.Body.Bytes(), &body))
	paths := make([]string, 0, len(body.Embedded.Facts))
	for _, f := range body.Embedded.Facts {
		paths = append(paths, f.Path)
	}
	return paths
}

// RegistryHasRow reports whether control.db still lists the repo as active.
func (a *controlApp) RegistryHasRow(name string) bool {
	a.t.Helper()
	reg := a.m.RepoRegistry()
	require.NotNil(a.t, reg, "the app must have a repo registry")
	_, found, err := reg.ActiveRecord(name)
	require.NoError(a.t, err)
	return found
}

// wipeRepoDatabases deletes every repos/*.db (and its -wal / -shm sidecars),
// leaving control.db and the agent key. This is the disaster the plan exists
// for: the volume holding the repo databases is gone, the control plane is not.
func (a *controlApp) wipeRepoDatabases() {
	a.t.Helper()
	reposDir := filepath.Join(a.home, "repos")
	entries, err := os.ReadDir(reposDir)
	require.NoError(a.t, err)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		require.NoError(a.t, os.Remove(filepath.Join(reposDir, e.Name())))
		removed++
	}
	require.NotZero(a.t, removed, "nothing was wiped — the test would prove nothing")
	require.FileExists(a.t, filepath.Join(a.home, "control.db"),
		"control.db must survive: it is the only record of what should exist")
	require.FileExists(a.t, filepath.Join(a.home, "id_ed25519"),
		"the agent key must survive: it decrypts the credentials control.db holds")
}

// TestControlDBAloneRebuildsEveryRepo is the acceptance criterion for moving
// origin credentials into the control plane.
//
// The private origin here demands HTTP basic auth and the test PROVES it does
// (an uncredentialed create against it fails). That is load-bearing: a file://
// or open-HTTP origin never authenticates, so a rebuild test against one passes
// even with the credential-copy deleted from Manager.rebuildSpec — the headline
// test would be blind to its own subject.
func TestControlDBAloneRebuildsEveryRepo(t *testing.T) {
	sb := testenv.NewStoryboard(t)

	private := sb.BareRemoteHTTPWithAuth("private", privateUser, privatePass)
	private.WriteMain("kb/gotchas/private-seed.md", testenv.Fact("private origin seed"),
		"seed the private origin")
	public := sb.BareRemoteHTTP("public")
	public.WriteMain("kb/gotchas/public-seed.md", testenv.Fact("public origin seed"),
		"seed the public origin")

	app := newControlApp(t, filepath.Join(t.TempDir(), "home"))

	// NON-VACUITY: the private origin genuinely requires a credential. Without
	// one the product cannot clone it at all, so every later success against it
	// is evidence the credential was recovered and used.
	noAuthErr := app.CreateFromOrigin("secure-without-credential", private.URL(), "", "")
	require.Error(t, noAuthErr,
		"the private origin must REQUIRE authentication — otherwise this whole test "+
			"passes with the credential-copy removed and proves nothing")
	t.Logf("uncredentialed clone of the private origin failed as required: %v", noAuthErr)

	require.NoError(t, app.CreateFromOrigin("secure", private.URL(),
		"basic", privateUser+":"+privatePass))
	require.NoError(t, app.CreateFromOrigin("open", public.URL(), "", ""))
	app.CreateLocal("localonly")
	app.WriteFact("localonly", "a fact that exists nowhere else")

	require.ElementsMatch(t, []string{"secure", "open", "localonly"}, app.APIRepoNames(),
		"all three repos must serve before the wipe")
	require.Contains(t, app.FactPaths("secure"), "kb/gotchas/private-seed.md",
		"the private repo's facts must be readable before the wipe")
	require.Contains(t, app.FactPaths("open"), "kb/gotchas/public-seed.md")
	require.NotEmpty(t, app.FactPaths("localonly"),
		"the origin-less repo must be serving facts before the wipe")

	app.Stop()
	app.wipeRepoDatabases()
	app.Start()

	// Both origin-backed repos are back. "secure" came back over a transport
	// that refuses anonymous access, using the credential control.db kept —
	// nobody re-entered it.
	require.ElementsMatch(t, []string{"secure", "open"}, app.APIRepoNames(),
		"a private token origin must be re-cloned from control.db with no re-authentication")
	require.FileExists(t, filepath.Join(app.home, "repos", "secure.db"))
	require.FileExists(t, filepath.Join(app.home, "repos", "open.db"))
	require.Contains(t, app.FactPaths("secure"), "kb/gotchas/private-seed.md",
		"the rebuilt private repo must serve the facts it cloned back from its origin")
	require.Contains(t, app.FactPaths("open"), "kb/gotchas/public-seed.md")

	// The origin-less repo cannot be rebuilt from anywhere, and must say so
	// rather than vanishing from the registry.
	require.Nil(t, app.m.Get("localonly"),
		"an origin-less repo whose database is gone has nothing to rebuild from")
	require.NoFileExists(t, filepath.Join(app.home, "repos", "localonly.db"))
	require.True(t, app.RegistryHasRow("localonly"),
		"an unrecoverable repo keeps its row so the state stays diagnosable")
}
