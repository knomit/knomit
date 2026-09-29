package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// F08 PR C: skills served as MCP prompts per binding, and by knomit_skill.
//
// Every request here goes through the REAL server's message entry point
// (MCPServer.HandleMessage), so the prompt hooks, mcp-go's own list/get
// handlers and the tool gates all run exactly as they do behind the HTTP
// mounts. The context carries what each mount's middleware would put there:
// a RepoInstance and branch (repo-scoped), the Binding repos.ResolveLensBinding
// mints from a persisted lens (lens), or the session-scoped marker (unscoped).

// skillFixture: alpha's consensus branch is "develop" (an origin names it),
// beta's is "main" — so a reader that spells "main" instead of asking
// UpstreamBranch() loses alpha's skills. The lens "eng" writes alpha and
// reads beta.
type skillFixture struct {
	m           *repos.Manager
	alpha, beta *repos.RepoInstance
}

func newSkillFixture(t *testing.T) *skillFixture {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	f := &skillFixture{m: m}
	f.alpha = newSkillRepo(t, m, "alpha", "develop")
	f.beta = newSkillRepo(t, m, "beta", "main")
	_, err := m.CreateLens(context.Background(), repos.Lens{
		Name: "eng", WriteUID: f.alpha.UID(), Reads: []repos.LensRead{{RepoUID: f.beta.UID()}},
	})
	require.NoError(t, err)
	return f
}

func newSkillRepo(t *testing.T, m *repos.Manager, name, upstream string) *repos.RepoInstance {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), name+".db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, upstream, "agent/test"))
	if upstream != "main" {
		svc.SetOrigin(&store.Origin{URL: "https://example.invalid/" + name + ".git", Branch: upstream})
	}
	require.Equal(t, upstream, svc.UpstreamBranch())
	uid := "uid-" + name
	require.NoError(t, m.Repos().Insert(repos.RepoRecord{
		UID: uid, Name: name, State: repos.StateActive, Profile: "code", CreatedAt: 1,
	}))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: name, UID: uid, AgentBranch: "agent/test", Svc: svc,
		Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	})
	m.Set(name, ri)
	return ri
}

// putFile commits one file of skill name on branch ("" = the repo's
// consensus branch, asked of the store, never spelled here).
func putFile(t *testing.T, ri *repos.RepoInstance, branch, name, file, content string) {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	defer release()
	if branch == "" {
		branch = svc.UpstreamBranch()
	}
	_, err = svc.Facts().WriteFact(context.Background(), branch, fact.SkillsDir+"/"+name+"/"+file, content, "skill "+name, "updated")
	require.NoError(t, err)
}

func skillMD(name, desc, body string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s", name, desc, body)
}

func putSkill(t *testing.T, ri *repos.RepoInstance, branch, name, desc, body string) {
	t.Helper()
	putFile(t, ri, branch, name, fact.SkillFileName, skillMD(name, desc, body))
}

// Contexts, as each mount's middleware builds them.
func repoCtx(ri *repos.RepoInstance) context.Context {
	return repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")
}

func (f *skillFixture) lensCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := repos.ResolveLensBinding(context.Background(), f.m, "eng")
	require.NoError(t, err)
	return ctx
}

func unscopedCtx() context.Context { return repos.WithSessionScoped(context.Background()) }

// rpcMsg sends one JSON-RPC request through srv.HandleMessage and returns the
// raw result, or the error message.
func rpcMsg(t *testing.T, srv *mcpserver.MCPServer, ctx context.Context, method string, params any) (json.RawMessage, string) {
	t.Helper()
	p, err := json.Marshal(params)
	require.NoError(t, err)
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, p)
	resp := srv.HandleMessage(ctx, json.RawMessage(msg))
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env), string(raw))
	if env.Error != nil {
		return nil, env.Error.Message
	}
	return env.Result, ""
}

type listedPrompt struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Arguments   []struct {
		Name     string `json:"name"`
		Required bool   `json:"required"`
	} `json:"arguments"`
}

func listPrompts(t *testing.T, srv *mcpserver.MCPServer, ctx context.Context) ([]listedPrompt, string) {
	t.Helper()
	raw, errMsg := rpcMsg(t, srv, ctx, "prompts/list", map[string]any{})
	require.Empty(t, errMsg, "prompts/list must never fail")
	var res struct {
		Prompts    []listedPrompt `json:"prompts"`
		NextCursor string         `json:"nextCursor"`
	}
	require.NoError(t, json.Unmarshal(raw, &res), string(raw))
	return res.Prompts, res.NextCursor
}

func promptNames(ps []listedPrompt) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

type gotMessage struct {
	Role    string `json:"role"`
	Content struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Resource struct {
			URI      string `json:"uri"`
			MIMEType string `json:"mimeType"`
			Text     string `json:"text"`
		} `json:"resource"`
	} `json:"content"`
}

type gotPrompt struct {
	Description string       `json:"description"`
	Messages    []gotMessage `json:"messages"`
}

func getPrompt(t *testing.T, srv *mcpserver.MCPServer, ctx context.Context, name string, args map[string]string) (gotPrompt, string) {
	t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	raw, errMsg := rpcMsg(t, srv, ctx, "prompts/get", params)
	var res gotPrompt
	if errMsg == "" {
		require.NoError(t, json.Unmarshal(raw, &res), string(raw))
	}
	return res, errMsg
}

// callSkill calls knomit_skill through tools/call; args is a JSON object.
func callSkill(t *testing.T, srv *mcpserver.MCPServer, ctx context.Context, args string) (string, bool) {
	t.Helper()
	raw, errMsg := rpcMsg(t, srv, ctx, "tools/call", map[string]any{"name": "knomit_skill", "arguments": json.RawMessage(args)})
	require.Empty(t, errMsg)
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	require.NoError(t, json.Unmarshal(raw, &res), string(raw))
	require.NotEmpty(t, res.Content)
	return res.Content[0].Text, res.IsError
}

// T-C1: two repos, each with its own `work-task`, on their own mounts: each
// lists exactly its own skills and gets its own body; a lens serves its write
// repo's skills only (N4).
func TestPrompts_ListedAndFetchedPerRepo(t *testing.T) {
	f := newSkillFixture(t)
	putSkill(t, f.alpha, "", "work-task", "Alpha's way.", "ALPHA body\n")
	putSkill(t, f.alpha, "", "alpha-only", "Only alpha.", "alpha only\n")
	putSkill(t, f.beta, "", "work-task", "Beta's way.", "BETA body\n")
	srv := NewServer("kb", f.m, false, nil)

	alpha, _ := listPrompts(t, srv, repoCtx(f.alpha))
	require.Equal(t, []string{"alpha-only", "work-task"}, promptNames(alpha))
	require.Equal(t, "Alpha's way.", alpha[1].Description)
	require.Len(t, alpha[1].Arguments, 1)
	require.Equal(t, "args", alpha[1].Arguments[0].Name)
	require.False(t, alpha[1].Arguments[0].Required)

	beta, _ := listPrompts(t, srv, repoCtx(f.beta))
	require.Equal(t, []string{"work-task"}, promptNames(beta), "beta must not list alpha's names")
	require.Equal(t, "Beta's way.", beta[0].Description)

	// Get beta FIRST, then alpha: whichever parsed first must not answer for
	// the other repo.
	got, errMsg := getPrompt(t, srv, repoCtx(f.beta), "work-task", nil)
	require.Empty(t, errMsg)
	require.Equal(t, "BETA body\n", got.Messages[0].Content.Text)
	require.Equal(t, "Beta's way.", got.Description)
	got, errMsg = getPrompt(t, srv, repoCtx(f.alpha), "work-task", nil)
	require.Empty(t, errMsg)
	require.Equal(t, "ALPHA body\n", got.Messages[0].Content.Text)
	require.Equal(t, "user", got.Messages[0].Role)

	// A name registered for alpha is still not beta's.
	_, errMsg = getPrompt(t, srv, repoCtx(f.beta), "alpha-only", nil)
	require.Contains(t, errMsg, "no skill alpha-only in repo beta")

	// The lens: the write repo's skills only, never the read mount's.
	lens, _ := listPrompts(t, srv, f.lensCtx(t))
	require.Equal(t, []string{"alpha-only", "work-task"}, promptNames(lens))
	got, errMsg = getPrompt(t, srv, f.lensCtx(t), "work-task", nil)
	require.Empty(t, errMsg)
	require.Equal(t, "ALPHA body\n", got.Messages[0].Content.Text)
}

// T-C2: skills come from the tip of the consensus branch (alpha's is
// "develop"), never the agent branch.
func TestPrompts_FromConsensusBranchNotAgentBranch(t *testing.T) {
	f := newSkillFixture(t)
	putSkill(t, f.alpha, "agent/test", "draft", "Unmerged.", "draft body\n")
	srv := NewServer("kb", f.m, false, nil)

	ps, _ := listPrompts(t, srv, repoCtx(f.alpha))
	require.Empty(t, ps, "a skill only on the agent branch is not served")
	_, errMsg := getPrompt(t, srv, repoCtx(f.alpha), "draft", nil)
	require.Contains(t, errMsg, "no skill draft in repo alpha")
	require.Contains(t, errMsg, "develop", "the error names the consensus branch it read")
	text, isErr := callSkill(t, srv, repoCtx(f.alpha), `{"name":"draft"}`)
	require.True(t, isErr)
	require.Contains(t, text, "no skill draft in repo alpha")

	// The consensus branch advances: now it is served.
	putSkill(t, f.alpha, "", "draft", "Merged.", "merged body\n")
	ps, _ = listPrompts(t, srv, repoCtx(f.alpha))
	require.Equal(t, []string{"draft"}, promptNames(ps))
	got, errMsg := getPrompt(t, srv, repoCtx(f.alpha), "draft", nil)
	require.Empty(t, errMsg)
	require.Equal(t, "merged body\n", got.Messages[0].Content.Text)
}

// T-C3: no skills folder is an empty list; an unknown name is a named error;
// a malformed SKILL.md is skipped and WARNed once per blob, never fatal.
func TestPrompts_AbsentAndMalformed(t *testing.T) {
	f := newSkillFixture(t)
	srv := NewServer("kb", f.m, false, nil)

	ps, _ := listPrompts(t, srv, repoCtx(f.beta))
	require.Empty(t, ps)
	_, errMsg := getPrompt(t, srv, repoCtx(f.beta), "nope", nil)
	require.Contains(t, errMsg, "no skill nope in repo beta")

	putSkill(t, f.beta, "", "good", "Fine.", "ok\n")
	putFile(t, f.beta, "", "no-front", fact.SkillFileName, "# no frontmatter\n")
	putFile(t, f.beta, "", "mismatch", fact.SkillFileName, skillMD("other", "d", "x"))
	putFile(t, f.beta, "", "no-desc", fact.SkillFileName, "---\nname: no-desc\n---\nx\n")

	logs := captureLogs(t)
	ps, _ = listPrompts(t, srv, repoCtx(f.beta))
	require.Equal(t, []string{"good"}, promptNames(ps))
	ps, _ = listPrompts(t, srv, repoCtx(f.beta))
	require.Equal(t, []string{"good"}, promptNames(ps))
	text, isErr := callSkill(t, srv, repoCtx(f.beta), `{}`)
	require.False(t, isErr, text)
	require.Contains(t, text, `"name":"good"`)
	require.NotContains(t, text, "mismatch")
	require.Equal(t, 3, strings.Count(logs.String(), "malformed skill skipped"),
		"one WARN per malformed blob, however often it is listed: %s", logs.String())

	_, errMsg = getPrompt(t, srv, repoCtx(f.beta), "mismatch", nil)
	require.Contains(t, errMsg, "skill mismatch in repo beta is malformed")
}

// T-C4: the unscoped mount has no repo: it lists no prompts — not even names
// another mount registered — and a get says to bind a repo.
func TestPrompts_UnscopedMountListsNone(t *testing.T) {
	f := newSkillFixture(t)
	putSkill(t, f.alpha, "", "work-task", "Alpha's way.", "ALPHA body\n")
	srv := NewServer("kb", f.m, false, nil)

	alpha, _ := listPrompts(t, srv, repoCtx(f.alpha)) // registers the name globally
	require.Equal(t, []string{"work-task"}, promptNames(alpha))

	ps, cursor := listPrompts(t, srv, unscopedCtx())
	require.Empty(t, ps, "the unscoped mount must not list the union of every repo's skills")
	require.Empty(t, cursor)
	_, errMsg := getPrompt(t, srv, unscopedCtx(), "work-task", nil)
	require.Contains(t, errMsg, "bind a repo")

	// knomit_skill there is gated like knomit_query: no handle, no answer.
	text, isErr := callSkill(t, srv, unscopedCtx(), `{}`)
	require.True(t, isErr)
	require.Contains(t, text, "knomit_bind")
}

// T-C5: bundled files: text inlined as embedded resources, binaries listed by
// path only, and the 256 KiB cap is a TOTAL across files.
func TestPrompts_BundledFilesInlined(t *testing.T) {
	f := newSkillFixture(t)
	putSkill(t, f.alpha, "", "pack", "Has files.", "Use ref.md.\n")
	putFile(t, f.alpha, "", "pack", "ref.md", "REF\n")
	putFile(t, f.alpha, "", "pack", "img.png", "\x89PNG\x00\x01binary")
	putFile(t, f.alpha, "", "pack", "scripts/run.sh", "echo hi\n")
	// Each under the cap, together over it.
	putSkill(t, f.alpha, "", "big", "Two big files.", "b\n")
	putFile(t, f.alpha, "", "big", "a.md", strings.Repeat("a", 200*1024))
	putFile(t, f.alpha, "", "big", "b.md", strings.Repeat("b", 100*1024))
	srv := NewServer("kb", f.m, false, nil)

	got, errMsg := getPrompt(t, srv, repoCtx(f.alpha), "pack", nil)
	require.Empty(t, errMsg)
	require.Len(t, got.Messages, 4, "body, two inlined files, one not-inlined line")
	require.Equal(t, "Use ref.md.\n", got.Messages[0].Content.Text)
	require.Equal(t, "resource", got.Messages[1].Content.Type)
	require.Equal(t, "knomit://alpha/.knomit/skills/pack/ref.md", got.Messages[1].Content.Resource.URI)
	require.Equal(t, "REF\n", got.Messages[1].Content.Resource.Text)
	require.Equal(t, "text/markdown", got.Messages[1].Content.Resource.MIMEType)
	require.Equal(t, "knomit://alpha/.knomit/skills/pack/scripts/run.sh", got.Messages[2].Content.Resource.URI)
	require.Equal(t, "text", got.Messages[3].Content.Type)
	require.Contains(t, got.Messages[3].Content.Text, "knomit://alpha/.knomit/skills/pack/img.png (binary")
	for _, m := range got.Messages {
		require.NotContains(t, m.Content.Resource.URI, "img.png", "a binary is never inlined")
	}

	got, errMsg = getPrompt(t, srv, repoCtx(f.alpha), "big", nil)
	require.Empty(t, errMsg)
	require.Len(t, got.Messages, 3)
	require.Equal(t, "knomit://alpha/.knomit/skills/big/a.md", got.Messages[1].Content.Resource.URI)
	require.Len(t, got.Messages[1].Content.Resource.Text, 200*1024)
	require.Contains(t, got.Messages[2].Content.Text, "knomit://alpha/.knomit/skills/big/b.md (over the 256 KiB inline cap")

	// knomit_skill applies the same rule.
	text, isErr := callSkill(t, srv, repoCtx(f.alpha), `{"name":"big"}`)
	require.False(t, isErr, text)
	var out skillGetResponse
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	require.Len(t, out.Files, 1)
	require.Equal(t, "a.md", out.Files[0].Path)
	require.Len(t, out.NotInlined, 1)
	require.Equal(t, "b.md", out.NotInlined[0].Path)
	require.Equal(t, "over-cap", out.NotInlined[0].Reason)
	require.Empty(t, out.NotInlined[0].Text)
}

// T-C6: $ARGUMENTS is replaced; without a placeholder the args are appended.
func TestPrompts_ArgumentsSubstituted(t *testing.T) {
	f := newSkillFixture(t)
	putSkill(t, f.alpha, "", "with-ph", "d", "Work on $ARGUMENTS now.\n")
	putSkill(t, f.alpha, "", "no-ph", "d", "Work.\n")
	srv := NewServer("kb", f.m, false, nil)

	got, _ := getPrompt(t, srv, repoCtx(f.alpha), "with-ph", map[string]string{"args": "task-7"})
	require.Equal(t, "Work on task-7 now.\n", got.Messages[0].Content.Text)
	got, _ = getPrompt(t, srv, repoCtx(f.alpha), "no-ph", map[string]string{"args": "task-7"})
	require.Equal(t, "Work.\n\nArguments: task-7\n", got.Messages[0].Content.Text)
	got, _ = getPrompt(t, srv, repoCtx(f.alpha), "with-ph", nil)
	require.Equal(t, "Work on  now.\n", got.Messages[0].Content.Text)
}

// T-C8 (N1): a fresh server answers a get with no list before it (a client
// reusing a list cached across a restart); and a list is one page, with no
// cursor, even when mcp-go paginated the global map.
func TestPrompts_GetBeforeList(t *testing.T) {
	f := newSkillFixture(t)
	putSkill(t, f.alpha, "", "work-task", "Alpha's way.", "ALPHA body\n")
	putSkill(t, f.alpha, "", "post-task", "Post.", "post\n")

	srv := NewServer("kb", f.m, false, nil)
	got, errMsg := getPrompt(t, srv, repoCtx(f.alpha), "work-task", nil)
	require.Empty(t, errMsg, "a get before any list must reach the skill")
	require.Equal(t, "ALPHA body\n", got.Messages[0].Content.Text)

	paged := newServer([]mcpserver.ServerOption{mcpserver.WithPaginationLimit(1)}, "kb", f.m, false, nil)
	ps, cursor := listPrompts(t, paged, repoCtx(f.alpha))
	require.Equal(t, []string{"post-task", "work-task"}, promptNames(ps), "the whole binding list in one page")
	require.Empty(t, cursor, "a cursor from the global pagination must not leak")
}

// knomit_skill: list and get through tools/call on a repo-scoped mount.
func TestSkillTool_ListAndGet(t *testing.T) {
	f := newSkillFixture(t)
	putSkill(t, f.alpha, "", "work-task", "Work one task.", "Do $ARGUMENTS.\n")
	putFile(t, f.alpha, "", "work-task", "checklist.md", "- [ ] one\n")
	putFile(t, f.alpha, "", "work-task", "logo.png", "\x00\x01")
	srv := NewServer("kb", f.m, false, nil)

	text, isErr := callSkill(t, srv, repoCtx(f.alpha), `{}`)
	require.False(t, isErr, text)
	var list skillListResponse
	require.NoError(t, json.Unmarshal([]byte(text), &list))
	require.Equal(t, "alpha", list.Repo)
	require.Equal(t, "develop", list.Branch)
	require.Len(t, list.Commit, 40)
	require.Equal(t, []skillListEntry{{Name: "work-task", Description: "Work one task."}}, list.Skills)

	text, isErr = callSkill(t, srv, repoCtx(f.alpha), `{"name":"work-task"}`)
	require.False(t, isErr, text)
	var got skillGetResponse
	require.NoError(t, json.Unmarshal([]byte(text), &got))
	require.Equal(t, "Do $ARGUMENTS.\n", got.Body)
	require.Equal(t, "Work one task.", got.Description)
	require.Len(t, got.Files, 1, "the bundled text file is returned")
	require.Equal(t, "checklist.md", got.Files[0].Path)
	require.Equal(t, "- [ ] one\n", got.Files[0].Text)
	require.Equal(t, "knomit://alpha/.knomit/skills/work-task/checklist.md", got.Files[0].URI)
	require.Len(t, got.NotInlined, 1)
	require.Equal(t, "binary", got.NotInlined[0].Reason)

	text, isErr = callSkill(t, srv, repoCtx(f.alpha), `{"name":"absent"}`)
	require.True(t, isErr)
	require.Contains(t, text, "no skill absent in repo alpha")

	text, isErr = callSkill(t, srv, repoCtx(f.alpha), `{"nmae":"x"}`)
	require.True(t, isErr, "unknown arguments are refused: %s", text)
}

// knomit_skill never lists across bindings: beta's skills are not alpha's,
// and a lens answers with its write repo's only.
func TestSkillTool_PerBinding(t *testing.T) {
	f := newSkillFixture(t)
	putSkill(t, f.alpha, "", "a-skill", "A.", "a\n")
	putSkill(t, f.beta, "", "b-skill", "B.", "b\n")
	srv := NewServer("kb", f.m, false, nil)

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"alpha", repoCtx(f.alpha), "a-skill"},
		{"beta", repoCtx(f.beta), "b-skill"},
		{"lens", f.lensCtx(t), "a-skill"},
	} {
		text, isErr := callSkill(t, srv, tc.ctx, `{}`)
		require.False(t, isErr, text)
		var list skillListResponse
		require.NoError(t, json.Unmarshal([]byte(text), &list))
		require.Equal(t, []skillListEntry{{Name: tc.want, Description: strings.ToUpper(tc.want[:1]) + "."}}, list.Skills, tc.name)
	}
	text, isErr := callSkill(t, srv, f.lensCtx(t), `{"name":"b-skill"}`)
	require.True(t, isErr, "a lens's read mount serves no skills: %s", text)
}
