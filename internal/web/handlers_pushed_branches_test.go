package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/repos"
	"knomit/internal/store"
)

const (
	pbRepo  = "alpha"
	pbAgent = "agent/test"
	pbPeer  = "agent/peer-99887766"
)

func pbFact(line string) string {
	return "---\ntype: observation\nconfidence: 0.9\n---\n# demo fact\n\n" + line + "\n"
}

// pushedServer is a store-backed repo with a peer branch forked from the
// agent branch, served through the FULL handler (edge, writeGate, Require).
func pushedServer(t *testing.T, s *Server) (http.Handler, *repos.Manager) {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch: pbAgent,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	newE2EMount(t, m, pbRepo, false)
	require.NoError(t, m.Get(pbRepo).WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), pbAgent, "kb/architecture/demo/aaaaaaaa.md", pbFact("original"), "seed", "learn")
		require.NoError(t, err)
		require.NoError(t, svc.Branches().CreateBranch(context.Background(), pbPeer, pbAgent))
	}))
	s.Manager = m
	s.OntologyRoot = "kb"
	return s.Handler(), m
}

func pbWrite(t *testing.T, m *repos.Manager, branch, path, line string) plumbing.Hash {
	t.Helper()
	var h plumbing.Hash
	require.NoError(t, m.Get(pbRepo).WithRead(func(svc *store.Service) {
		res, err := svc.Facts().WriteFact(context.Background(), branch, path, pbFact(line), "w", "update")
		require.NoError(t, err)
		h = plumbing.NewHash(res.CommitHash)
	}))
	return h
}

func pbHead(t *testing.T, m *repos.Manager, branch string) string {
	t.Helper()
	var h string
	require.NoError(t, m.Get(pbRepo).WithRead(func(svc *store.Service) {
		var err error
		h, err = svc.Branches().HeadCommit(context.Background(), branch)
		require.NoError(t, err)
	}))
	return h
}

func pbBody(t *testing.T, m *repos.Manager, branch, path string) string {
	t.Helper()
	var body string
	require.NoError(t, m.Get(pbRepo).WithRead(func(svc *store.Service) {
		f, err := svc.Facts().ReadFact(context.Background(), branch, path, nil)
		require.NoError(t, err)
		body = f.Content
	}))
	return body
}

func pbPost(t *testing.T, h http.Handler, branch, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := fromLoopback(httptest.NewRequest(http.MethodPost,
		"/api/v1/repos/"+pbRepo+"/branches/"+strings.ReplaceAll(branch, "/", ":")+"/merge", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func pbList(t *testing.T, h http.Handler) []map[string]any {
	t.Helper()
	req := fromLoopback(httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+pbRepo+"/pushed-branches", nil))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var body struct {
		Embedded struct {
			Items []map[string]any `json:"pushed_branches"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	return body.Embedded.Items
}

func expect(tip string) string { return `{"expected_tip":"` + tip + `"}` }

// The list shows the peer branch with what it would bring; the merge writes a
// host merge commit whose second parent is the reviewed tip; the list then
// shows nothing left to merge.
func TestPushedBranches_ListAndMerge(t *testing.T) {
	h, m := pushedServer(t, &Server{})
	peerTip := pbWrite(t, m, pbPeer, "kb/architecture/demo/bbbbbbbb.md", "from the peer")
	hostBefore := pbHead(t, m, pbAgent)

	items := pbList(t, h)
	require.Len(t, items, 1, "only the pushed branch is listed, not the agent branch")
	require.Equal(t, pbPeer, items[0]["name"])
	require.Equal(t, peerTip.String(), items[0]["tip"])
	require.EqualValues(t, 1, items[0]["to_merge"])
	require.Equal(t, false, items[0]["in_agent_branch"])
	_, hasUpstreamCol := items[0]["merged_in_upstream"]
	require.False(t, hasUpstreamCol, "D4a: no main-on-origin field")

	rr := pbPost(t, h, pbPeer, expect(peerTip.String()))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var res map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
	require.Equal(t, "merge", res["mode"])
	require.Equal(t, pbAgent, res["into"], "the target is the agent branch, always")
	newTip := pbHead(t, m, pbAgent)
	require.Equal(t, newTip, res["new_tip"], "pin: the reported tip IS the agent branch head (the chokepoint moved it)")
	require.NotEqual(t, hostBefore, newTip)
	require.Contains(t, pbBody(t, m, pbAgent, "kb/architecture/demo/bbbbbbbb.md"), "from the peer")
	require.Equal(t, peerTip.String(), pbHead(t, m, pbPeer), "the peer branch is untouched")

	items = pbList(t, h)
	require.EqualValues(t, 0, items[0]["to_merge"])
	require.Equal(t, true, items[0]["in_agent_branch"])
}

// Test 6 (N6): two gates, each pinned by the case only it refuses. An
// instance certificate lacks `write` (writeGate); an operator certificate
// granted `write` but not `operator` is refused by Require(operator).
func TestPushedMerge_PermissionGates(t *testing.T) {
	f := pkitest.New(t)
	inst := f.Enroll(t, "peer", pki.RoleInstance)
	op := f.Enroll(t, "ops", pki.RoleOperator)
	opPrincipal := auth.OperatorPrincipal(op.Fingerprint()).String()
	grants := auth.StaticGrants{opPrincipal: {auth.Read: {}, auth.Write: {}}}
	s := &Server{Auth: config.AuthConfig{LoopbackDefault: config.Defaults().Auth.LoopbackDefault}, Grants: grants}
	h, m := pushedServer(t, s)
	peerTip := pbWrite(t, m, pbPeer, "kb/architecture/demo/bbbbbbbb.md", "p")
	hostBefore := pbHead(t, m, pbAgent)

	rr := pbPost(t, asTLSPeer(h, inst.Cert), pbPeer, expect(peerTip.String()))
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "requires the write permission", "an instance is refused by writeGate")

	rr = pbPost(t, asTLSPeer(h, op.Cert), pbPeer, expect(peerTip.String()))
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "requires the operator permission", "write without operator is refused by Require(operator)")
	require.Equal(t, hostBefore, pbHead(t, m, pbAgent))

	grants[opPrincipal] = auth.Set{auth.Read: {}, auth.Write: {}, auth.Operator: {}}
	rr = pbPost(t, asTLSPeer(h, op.Cert), pbPeer, expect(peerTip.String()))
	require.Equal(t, http.StatusOK, rr.Code, "granted operator, the same certificate merges: %s", rr.Body.String())
}

// Test 7: every refusal names itself and moves nothing.
func TestPushedMerge_Refusals(t *testing.T) {
	h, m := pushedServer(t, &Server{})
	peerTip := pbWrite(t, m, pbPeer, "kb/architecture/demo/bbbbbbbb.md", "p")
	require.NoError(t, m.Get(pbRepo).WithRead(func(svc *store.Service) {
		ctx := context.Background()
		_, err := svc.Experiments().OpenExperiment(ctx, "try", "", pbAgent)
		require.NoError(t, err)
	}))
	hostBefore := pbHead(t, m, pbAgent)
	tip := peerTip.String()

	for _, c := range []struct {
		name, branch, body string
		code               int
		title              string
	}{
		{"missing expected_tip", pbPeer, `{}`, 400, "Invalid request"},
		{"short expected_tip", pbPeer, expect(tip[:12]), 400, "Invalid request"},
		{"ours is not a resolution", pbPeer, `{"expected_tip":"` + tip + `","resolution":"ours"}`, 400, "Invalid resolution"},
		{"theirs is not a resolution", pbPeer, `{"expected_tip":"` + tip + `","resolution":"theirs"}`, 400, "Invalid resolution"},
		{"no such branch", "agent/nobody-12345678", expect(tip), 404, "Branch not found"},
		{"the agent branch itself", pbAgent, expect(tip), 422, "Not a pushed branch"},
		{"an experiment", "exp/try", expect(tip), 422, "Not a pushed branch"},
		{"branch moved", pbPeer, expect(strings.Repeat("a", 40)), 409, "Branch moved"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := pbPost(t, h, c.branch, c.body)
			require.Equal(t, c.code, rr.Code, rr.Body.String())
			require.Contains(t, rr.Body.String(), c.title)
			require.Equal(t, hostBefore, pbHead(t, m, pbAgent), "a refusal moves nothing")
		})
	}
}

// Test 3 (M5) and test 8 (N8): a conflict is a 409 carrying the paths and the
// three commits under host/peer names; "host" keeps the host's body, "peer"
// takes the peer's.
func TestPushedMerge_ConflictAndResolution(t *testing.T) {
	const path = "kb/architecture/demo/aaaaaaaa.md"
	for _, c := range []struct{ resolution, want string }{{"host", "host version"}, {"peer", "peer version"}} {
		t.Run(c.resolution, func(t *testing.T) {
			h, m := pushedServer(t, &Server{})
			peerTip := pbWrite(t, m, pbPeer, path, "peer version")
			hostTip := pbWrite(t, m, pbAgent, path, "host version")

			rr := pbPost(t, h, pbPeer, expect(peerTip.String()))
			require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
			var prob map[string]any
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &prob))
			require.Equal(t, []any{path}, prob["conflicting_paths"])
			require.Equal(t, hostTip.String(), prob["host_commit"])
			require.Equal(t, peerTip.String(), prob["peer_commit"])
			require.NotEmpty(t, prob["base_commit"])
			require.Equal(t, hostTip.String(), pbHead(t, m, pbAgent), "a refused merge moves nothing")

			rr = pbPost(t, h, pbPeer, `{"expected_tip":"`+peerTip.String()+`","resolution":"`+c.resolution+`"}`)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			require.Contains(t, pbBody(t, m, pbAgent, path), c.want)
		})
	}
}

// A subscription has no agent branch: nothing is listed and nothing merges.
func TestPushedBranches_Subscription(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{Cfg: config.Config{Home: t.TempDir(), OntologyRoot: "kb"}})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	newE2EMount(t, m, pbRepo, true)
	s := &Server{Manager: m, OntologyRoot: "kb"}
	h := s.Handler()

	require.Empty(t, pbList(t, h))
	rr := pbPost(t, h, pbPeer, expect(strings.Repeat("a", 40)))
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "Repository has no agent branch")
}
