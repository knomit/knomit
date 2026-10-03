package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// seedFleetBare is a bare fleet repository whose main holds the fleet preset.
func seedFleetBare(t *testing.T, dir string) string {
	t.Helper()
	bare := filepath.Join(dir, "fleet.git")
	gitRun(t, dir, "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	gitRun(t, dir, "clone", bare, work)
	ont, err := fact.FleetOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(repos.OntologyPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, repos.OntologyPath), ont, 0o644))
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-m", "fleet")
	gitRun(t, work, "push", "origin", "main")
	return fileuri.New(bare)
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
	url := seedFleetBare(t, dir)
	home := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: dir},
		AgentBranch: "agent/rest-fleet", Signer: testsigner.Named("rest-fleet"), Machine: repos.Options{Synchronous: true},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	s := &Server{Manager: m}

	out := getFleetJSON(t, s)
	require.Equal(t, []any{}, out["external_addresses"], "unset is an empty array, never null")
	require.Contains(t, out["notice"], "external_addresses")
	require.NotContains(t, out, "record")

	_, err := m.RegisterFleet(context.Background(), url, "", "")
	require.NoError(t, err)
	out = getFleetJSON(t, s)
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

// F10: a member row shows every advertised field as the record has it, even
// someone else's odd values; addresses is always an array.
func TestFleetMemberRow(t *testing.T) {
	row := fleetMemberRow(store.FleetMember{Member: fact.Member{
		Agent: "peer", State: fact.MemberActive, Host: "h", Addresses: []string{"not-a-url"}, Git: "/git",
		Capabilities: map[string]string{"os": "plan9"},
	}, Path: "kb/members/peer/x.md"})
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	require.JSONEq(t, `{"agent":"peer","state":"active","host":"h","branch":"","path":"kb/members/peer/x.md",
		"addresses":["not-a-url"],"git":"/git","capabilities":{"os":"plan9"}}`, string(raw))

	raw, err = json.Marshal(fleetMemberRow(store.FleetMember{Member: fact.Member{Agent: "old", State: fact.MemberActive}}))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, []any{}, m["addresses"], "an F09 record without addresses is []")
	require.Equal(t, map[string]any{}, m["capabilities"])
}
