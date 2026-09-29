package web

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/repos"
)

// F08 PR C over the REAL router and MCP server: skills as prompts on the
// URL-scoped mounts, none on the unscoped mount, and knomit_skill on the
// unscoped mount answering for the handle's binding like knomit_query.

func skillsE2E(t *testing.T) (http.Handler, *repos.Manager) {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	newE2EMount(t, m, "alpha", false)
	newE2EMount(t, m, "followed", true)
	st := newClientSessionsStore(t)
	m.SetClientSessions(st)
	s := &Server{Manager: m, ClientSessions: st, OntologyRoot: "kb"}
	s.buildMCPHandler()
	return s.NewAPIRouter(), m
}

func putE2ESkill(t *testing.T, ri *repos.RepoInstance, name, body string) {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	defer release()
	src := fmt.Sprintf("---\nname: %s\ndescription: %s skill\n---\n%s", name, name, body)
	_, err = svc.Facts().WriteFact(context.Background(), svc.UpstreamBranch(), fact.SkillPath(name), src, "skill", "updated")
	require.NoError(t, err)
}

func promptNamesAt(t *testing.T, h http.Handler, mount, sid string) []string {
	t.Helper()
	resp, _ := rpcAt(t, h, mount, sid, `{"jsonrpc":"2.0","id":2,"method":"prompts/list","params":{}}`)
	result, ok := resp["result"].(map[string]any)
	require.True(t, ok, "no result in %v", resp)
	names := []string{}
	ps, _ := result["prompts"].([]any)
	for _, p := range ps {
		names = append(names, p.(map[string]any)["name"].(string))
	}
	return names
}

func TestSkillsMCP_MountsOverHTTP(t *testing.T) {
	h, m := skillsE2E(t)
	putE2ESkill(t, m.Get("alpha"), "work-task", "ALPHA\n")
	// A subscription has no agent branch; its consensus branch still serves.
	putE2ESkill(t, m.Get("followed"), "read-only-skill", "FOLLOWED\n")

	const alphaMount = "/repos/alpha/branches/agent%2Ftest/mcp"
	const followedMount = "/repos/followed/branches/main/mcp"

	sid := initAt(t, h, alphaMount)
	require.Equal(t, []string{"work-task"}, promptNamesAt(t, h, alphaMount, sid))
	resp, _ := rpcAt(t, h, alphaMount, sid, `{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"work-task"}}`)
	require.Contains(t, fmt.Sprint(resp["result"]), "ALPHA")

	fsid := initAt(t, h, followedMount)
	require.Equal(t, []string{"read-only-skill"}, promptNamesAt(t, h, followedMount, fsid))

	// The unscoped mount: no prompts at all, though both names are registered.
	usid := initAt(t, h, unscopedMount)
	require.Empty(t, promptNamesAt(t, h, unscopedMount, usid))
	resp, _ = rpcAt(t, h, unscopedMount, usid, `{"jsonrpc":"2.0","id":4,"method":"prompts/get","params":{"name":"work-task"}}`)
	require.Contains(t, fmt.Sprint(resp["error"]), "bind a repo")

	// knomit_skill there: no handle → the gate's refusal; a handle → that
	// binding's skills.
	text, isErr := callToolAt(t, h, unscopedMount, usid, "knomit_skill", `{}`)
	require.True(t, isErr, text)
	require.Contains(t, text, "knomit_bind")
	handle := bindHandle(t, h, usid, "followed")
	text, isErr = callToolAt(t, h, unscopedMount, usid, "knomit_skill", fmt.Sprintf(`{"binding":%q}`, handle))
	require.False(t, isErr, text)
	require.Contains(t, text, `"name":"read-only-skill"`)
	require.NotContains(t, text, "work-task")
	text, isErr = callToolAt(t, h, unscopedMount, usid, "knomit_skill", fmt.Sprintf(`{"binding":%q,"name":"read-only-skill"}`, handle))
	require.False(t, isErr, text)
	require.Contains(t, text, "FOLLOWED")
}
