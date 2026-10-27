package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki"
	"knomit/internal/foundation/pki/pkitest"
	"knomit/internal/platform/fileuri"
	"knomit/internal/testsupport/testsigner"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	// maintenance.auto=false: no detached auto-maintenance in a work clone
	// under t.TempDir (#651; seedFleetBare sets it on the bare remote).
	cmd := exec.Command("git", append([]string{"-c", "maintenance.auto=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// seedFleetBare is a bare fleet repository whose main holds the fleet
// ontology and a fresh root's root.crt and CRL (what a forge create leaves),
// and that root.
func seedFleetBare(t *testing.T, dir string) (string, *pkitest.Fleet) {
	t.Helper()
	root := pkitest.New(t)
	bare := filepath.Join(dir, "fleet.git")
	gitRun(t, dir, "init", "--bare", "--initial-branch=main", bare)
	// #651: every push into bare runs git-receive-pack, which would start a
	// detached `git maintenance run --auto` holding objects/maintenance.lock
	// past the push; t.TempDir's RemoveAll then raced it ("unlinkat
	// .../fleet.git: directory not empty"). receive-pack reads this setting
	// in bare itself and starts no child.
	gitRun(t, dir, "-C", bare, "config", "maintenance.auto", "false")
	work := t.TempDir()
	gitRun(t, dir, "clone", bare, work)
	ont, err := fact.FleetOntology().Serialize()
	require.NoError(t, err)
	files := map[string][]byte{repos.OntologyPath: ont}
	for _, name := range []string{pki.RootCertFile, pki.CRLFile} {
		b, err := os.ReadFile(filepath.Join(root.Dir, name))
		require.NoError(t, err)
		files["artifacts/fleet/"+name] = b
	}
	for p, b := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(work, p), b, 0o644))
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-m", "fleet")
	gitRun(t, work, "push", "origin", "main")
	return fileuri.New(bare), root
}

// restFleetManager is a started manager named rest-fleet whose key file
// holds its signing key, with its trust material in <home>/pki.
func restFleetManager(t *testing.T, dir string) (*repos.Manager, string) {
	t.Helper()
	home := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	key, err := testsigner.WriteNamedKey(home, "rest-fleet")
	require.NoError(t, err)
	m := repos.New(context.Background(), repos.Deps{
		Cfg: config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: dir,
			TLS: config.TLSConfig{Dir: filepath.Join(home, "pki")}},
		AgentID: testsigner.FixtureAgentID("agent/rest-fleet"), AgentBranch: "agent/rest-fleet", Signer: testsigner.Named("rest-fleet"), KeyPath: key, Machine: repos.Options{Synchronous: true},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	return m, key
}

// localWriteGrants: the socket principal holds read and write, as an
// operator's grant gives it, so a local-only route also passes the write
// gate.
func localWriteGrants() auth.StaticGrants {
	local := auth.Principal{Kind: auth.KindBridge, ID: "uid:501", Via: auth.LocalVia}
	return auth.StaticGrants{local.String(): {auth.Read: {}, auth.Write: {}}}
}

// localOperatorGrants is the desktop app's principal on the local listener
// with what the operator holds there: read, write, operator and admin. The
// fleet operations (/fleet) are served to it only.
func localOperatorGrants() auth.StaticGrants {
	local := auth.Principal{Kind: auth.KindBridge, ID: "uid:501", Via: auth.LocalVia}
	return auth.StaticGrants{local.String(): {auth.Read: {}, auth.Write: {}, auth.Operator: {}, auth.Admin: {}}}
}

// joinOverSocket is POST /fleet/join over the local listener with a bundle
// root issues for the manager's key and agent id.
func joinOverSocket(t *testing.T, s *Server, key, url string, root *pkitest.Fleet) *httptest.ResponseRecorder {
	t.Helper()
	bundle := root.Bundle(t, root.Enroll(t, "rest-fleet@"+pki.AgentEmailDomain, key))
	body, err := json.Marshal(map[string]string{"bundle": string(pkitest.WithURL(t, bundle, url))})
	require.NoError(t, err)
	req := localPeer(httptest.NewRequest(http.MethodPost, "/fleet/join", strings.NewReader(string(body))))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, req)
	return rec
}

func getFleetJSON(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fleet", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// F10: GET /api/v1/fleet carries the configured external_addresses ([] when
// unset, with the notice), and once registered the own record's advertised
// fields and whether they are current.
func TestFleetREST_AddressesAndRecord(t *testing.T) {
	dir := t.TempDir()
	url, root := seedFleetBare(t, dir)
	m, key := restFleetManager(t, dir)
	s := &Server{Manager: m, Grants: localWriteGrants()}

	out := getFleetJSON(t, s)
	require.Equal(t, map[string]any{"listener_on": false, "listening": false, "external_addresses": []any{}}, out["host"], "unset is an empty array, never null")
	require.Contains(t, out["notice"], "external_addresses")
	require.NotContains(t, out, "record")
	require.NotContains(t, out, "fleet_url", "fleet_url is gone: fleet_urls")
	require.Equal(t, []any{}, out["fleet_urls"], "standalone: no fleet URL")
	require.NotContains(t, out, "root_fingerprint", "no root installed")
	require.Equal(t, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(testsigner.Named("rest-fleet").PublicKey()))), out["public_key"])

	join := joinOverSocket(t, s, key, url, root)
	require.Equal(t, http.StatusOK, join.Code, join.Body.String())
	var joined map[string]any
	require.NoError(t, json.Unmarshal(join.Body.Bytes(), &joined))
	rootID, err := pki.RootID(root.Root.Cert)
	require.NoError(t, err)
	require.Equal(t, rootID, joined["root_fingerprint"], "the join names the root it installed (review N7)")
	out = getFleetJSON(t, s)
	require.Equal(t, repos.FleetRegistered, out["state"])
	require.Equal(t, false, out["master"])
	require.Equal(t, url, out["hosting"])
	require.Equal(t, []any{url}, out["fleet_urls"], "main holds no host record: the origin")
	require.NotContains(t, out, "host_record")
	require.Equal(t, rootID, out["root_fingerprint"], "the installed root")
	require.Equal(t, filepath.Join(dir, "home", "fleet"), out["root_dir"])
	rec, ok := out["record"].(map[string]any)
	require.True(t, ok, "registered: the own record is shown: %v", out)
	require.Equal(t, []any{}, rec["addresses"])
	caps, ok := rec["capabilities"].(map[string]any)
	require.True(t, ok)
	require.NotEmpty(t, caps["os"])
	require.NotEmpty(t, caps["arch"])
	require.NotEmpty(t, caps["version"])
	require.Equal(t, "false", caps["read_only"])
	require.Equal(t, true, out["record_current"])
	require.Equal(t, false, out["record_pending_update"])
}

// GET /fleet's `local` is the edge's verdict on how the request came in, the
// same test httpx.RequireLocalVia makes: true over the local listener, false over
// loopback TCP however the client dresses the request up.
// SABOTAGE: set local from a request header -> the spoofed row is true ->
// red; set it true always -> the TCP row is true -> red.
func TestFleetREST_LocalIsTheListenerNotTheClient(t *testing.T) {
	m, _ := restFleetManager(t, t.TempDir())
	s := &Server{Manager: m, Grants: localWriteGrants()}
	get := func(req *http.Request) map[string]any {
		rec := httptest.NewRecorder()
		s.NewAPIRouter().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	require.Equal(t, true, get(localPeer(httptest.NewRequest(http.MethodGet, "/fleet", nil)))["local"], "over the local listener")
	require.Equal(t, false, get(fromLoopback(httptest.NewRequest(http.MethodGet, "/fleet", nil)))["local"], "over loopback TCP")
	spoofed := fromLoopback(httptest.NewRequest(http.MethodGet, "/fleet?local=true", nil))
	spoofed.Header.Set("X-Knomit-Via", string(auth.LocalVia))
	spoofed.Header.Set("X-Forwarded-For", "unix")
	spoofed.Header.Set("Origin", "app://knomit")
	require.Equal(t, false, get(spoofed)["local"], "nothing the client sends makes it local")
}

// GET /fleet's host.listener (the [tls].addr bind) is shown to a local caller
// only; over TCP the page learns that the listener is on (listener_on), not
// where it is bound (PR #527).
// SABOTAGE: drop the delete in fleetView -> the TCP row has the bind -> red;
// drop listener_on -> red.
func TestFleetREST_ListenerIsShownOnlyLocally(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	key, err := testsigner.WriteNamedKey(home, "rest-fleet")
	require.NoError(t, err)
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: home, OntologyRoot: "kb", TLS: config.TLSConfig{Dir: filepath.Join(home, "pki"), Addr: "0.0.0.0:7443"}},
		AgentID: testsigner.FixtureAgentID("agent/rest-fleet"), AgentBranch: "agent/rest-fleet", Signer: testsigner.Named("rest-fleet"), KeyPath: key, Machine: repos.Options{Synchronous: true},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	s := &Server{Manager: m, Grants: localWriteGrants()}
	get := func(req *http.Request) map[string]any {
		rec := httptest.NewRecorder()
		s.NewAPIRouter().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	local := get(localPeer(httptest.NewRequest(http.MethodGet, "/fleet", nil)))["host"].(map[string]any)
	require.Equal(t, "0.0.0.0:7443", local["listener"], "a local caller sees the bind")
	require.Equal(t, true, local["listener_on"])
	tcp := get(fromLoopback(httptest.NewRequest(http.MethodGet, "/fleet", nil)))["host"].(map[string]any)
	require.NotContains(t, tcp, "listener", "over TCP the bind is not shown")
	require.Equal(t, true, tcp["listener_on"], "only that the listener is on")
}
