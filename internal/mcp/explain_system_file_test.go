package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// Reads of .knomit/ by exact path (user ruling 2026-10-06: "reopen reads by
// path"; "if we cannot get to the js file now because it's not a fact, let's
// fix it"). knomit_explain returns a system file raw when named directly, and
// a fact's refs to .knomit/ files appear in its walk as summary nodes pinned
// at the fact's commit. Writes to .knomit/ stay refused (three_roots_test.go).

const (
	sysClaims   = ".knomit/templates/mission/.knomit/triggers/claims.js"
	sysSkill    = ".knomit/skills/program-knomit/SKILL.md"
	sysOntology = ".knomit/ontology.yaml"
	sysBinary   = ".knomit/bin/blob.bin"
	sysBig      = ".knomit/runs/big.txt"
	sysState    = ".knomit/runs/state.txt"
)

var sysFiles = map[string]string{
	sysClaims:   "const WINDOW_SECONDS = 30;\nexport function offer() {}\n",
	sysSkill:    "---\nname: program-knomit\ndescription: How to program knomit.\n---\nBody of the skill.\n",
	sysOntology: "topics:\n  architecture: {}\n",
	sysBinary:   "\x00\xff\xfe\x01not utf-8",
	sysBig:      strings.Repeat("a", explainFileContentMax+1),
	sysState:    "v1\n",
}

// sysFileRepo is a repo whose initial tree carries sysFiles, written the way
// git writes them (case kept: the store's fact writer would lowercase
// SKILL.md).
func sysFileRepo(t *testing.T) (*repos.RepoInstance, context.Context) {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), sysFiles, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "sys", UID: nextTestRepoUID(), AgentBranch: "agent/test", Svc: svc,
		Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	})
	return ri, repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")
}

// fileNode is the explain JSON of a system_file node.
type fileNode struct {
	Path             string  `json:"path"`
	Commit           string  `json:"commit"`
	Depth            int     `json:"depth"`
	Kind             string  `json:"kind"`
	Blob             string  `json:"blob"`
	Size             int64   `json:"size"`
	Content          *string `json:"content"`
	Encoding         string  `json:"encoding"`
	ContentTruncated bool    `json:"content_truncated"`
	Superseded       bool    `json:"superseded"`
	Deleted          bool    `json:"deleted"`
	Summary          bool    `json:"summary"`
	Title            *string `json:"title"`
}

func explainFileDirect(t *testing.T, ctx context.Context, file, commit string) fileNode {
	t.Helper()
	args := map[string]any{"file": file}
	if commit != "" {
		args["commit"] = commit
	}
	text, isErr := callExplain(t, ctx, args)
	require.Falsef(t, isErr, "explain %s: %s", file, text)
	var resp struct {
		Facts   []fileNode `json:"facts"`
		Cursor  *string    `json:"cursor"`
		HasMore bool       `json:"has_more"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp), text)
	require.Len(t, resp.Facts, 1, "a file explained directly is ONE node: %s", text)
	require.Nil(t, resp.Cursor, "no walk, no cursor")
	require.False(t, resp.HasMore)
	return resp.Facts[0]
}

// explainWalk drives explain through every page and returns each node as raw
// JSON keyed by path.
func explainWalk(t *testing.T, ctx context.Context, file string) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	args := map[string]any{"file": file}
	for {
		text, isErr := callExplain(t, ctx, args)
		require.False(t, isErr, text)
		var resp struct {
			Facts   []json.RawMessage `json:"facts"`
			Cursor  *string           `json:"cursor"`
			HasMore bool              `json:"has_more"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &resp), text)
		for _, raw := range resp.Facts {
			var head struct {
				Path string `json:"path"`
			}
			require.NoError(t, json.Unmarshal(raw, &head))
			out[head.Path] = raw
		}
		if !resp.HasMore || resp.Cursor == nil {
			return out
		}
		args = map[string]any{"file": file, "cursor": *resp.Cursor}
	}
}

func asFileNode(t *testing.T, raw json.RawMessage) fileNode {
	t.Helper()
	require.NotNil(t, raw)
	var n fileNode
	require.NoError(t, json.Unmarshal(raw, &n))
	return n
}

// TestExplain_SystemFileByPath: yaml, js and SKILL.md come back raw, byte for
// byte, path and case kept, as one system_file node with no fact fields; a
// non-UTF-8 file comes back base64; a file over the cap has no content; the
// wrong case, a directory and a missing file are "not found" naming the path
// and the branch.
// Sabotage: route the system path through NormalizePath first (the pre-change
// order) → ".knomit/…/skill.md.md" is refused as closed → red.
func TestExplain_SystemFileByPath(t *testing.T) {
	ri, ctx := sysFileRepo(t)
	head := headOf(t, ri, "agent/test")

	for _, p := range []string{sysOntology, sysClaims, sysSkill} {
		n := explainFileDirect(t, ctx, p, "")
		require.Equal(t, p, n.Path, "path verbatim, case kept")
		require.Equal(t, "system_file", n.Kind)
		require.Equal(t, 0, n.Depth)
		require.Equal(t, head, n.Commit)
		require.Len(t, n.Blob, 40)
		require.EqualValues(t, len(sysFiles[p]), n.Size)
		require.NotNil(t, n.Content)
		require.Equal(t, sysFiles[p], *n.Content, "raw content of %s", p)
		require.Empty(t, n.Encoding)
		require.False(t, n.Summary)
		require.False(t, n.Superseded)
		require.False(t, n.Deleted)
		require.Nil(t, n.Title, "a file has no title: it is not a fact")
	}

	bin := explainFileDirect(t, ctx, sysBinary, "")
	require.Equal(t, "base64", bin.Encoding)
	require.NotNil(t, bin.Content)
	dec, err := base64.StdEncoding.DecodeString(*bin.Content)
	require.NoError(t, err)
	require.Equal(t, sysFiles[sysBinary], string(dec))

	big := explainFileDirect(t, ctx, sysBig, "")
	require.True(t, big.ContentTruncated)
	require.Nil(t, big.Content)
	require.EqualValues(t, explainFileContentMax+1, big.Size)

	for _, p := range []string{".knomit/skills/program-knomit/skill.md", ".knomit/skills/program-knomit", ".knomit/nope.yaml"} {
		text, isErr := callExplain(t, ctx, map[string]any{"file": p})
		require.Truef(t, isErr, "explain %s: %s", p, text)
		require.Containsf(t, text, p+" not found at the tip of branch agent/test", "explain %s", p)
	}
}

// TestExplain_WalksFactToFile: template fact → file fact → file node, in one
// cursor walk. The file node is a summary pinned at the FILE FACT's commit
// (the referrer), with blob and size but no content; explaining it with that
// path and commit returns the content.
// Sabotage: drop the systemFileChildren loop in explainResume → the depth-2
// file node never appears → red.
func TestExplain_WalksFactToFile(t *testing.T) {
	ri, ctx := sysFileRepo(t)
	fileFactCommit := writeExplainFact(t, ctx, ri, "kb/templates/mission/files/claims.md", "claims.js file fact", 0.9,
		[]string{sysClaims})
	writeExplainFact(t, ctx, ri, "kb/templates/mission/root.md", "mission template", 0.9,
		[]string{"kb/templates/mission/files/claims.md", sysOntology})

	nodes := explainWalk(t, ctx, "kb/templates/mission/root.md")

	var root expFact
	require.NoError(t, json.Unmarshal(nodes["kb/templates/mission/root.md"], &root))
	require.Equal(t, 0, root.Depth)

	// The root's own file ref: a summary at depth 1, pinned at the root's commit.
	onto := asFileNode(t, nodes[sysOntology])
	require.Equal(t, 1, onto.Depth)
	require.True(t, onto.Summary)
	require.Equal(t, root.Commit, onto.Commit)
	require.Nil(t, onto.Content, "a file deeper than the root is a summary (decision 1)")

	// The file fact, then its file at depth 2.
	var ff expFact
	require.NoError(t, json.Unmarshal(nodes["kb/templates/mission/files/claims.md"], &ff))
	require.Equal(t, 1, ff.Depth)
	require.True(t, ff.Summary)

	claims := asFileNode(t, nodes[sysClaims])
	require.Equal(t, "system_file", claims.Kind)
	require.Equal(t, 2, claims.Depth)
	require.True(t, claims.Summary)
	require.Equal(t, fileFactCommit, claims.Commit, "pinned at the referrer's commit (decision 3)")
	require.Len(t, claims.Blob, 40)
	require.EqualValues(t, len(sysFiles[sysClaims]), claims.Size)
	require.Nil(t, claims.Content)
	require.False(t, claims.Superseded)
	require.False(t, claims.Deleted)

	// Drill in: the same path and commit returns the content.
	full := explainFileDirect(t, ctx, claims.Path, claims.Commit)
	require.Equal(t, sysFiles[sysClaims], *full.Content)
	require.Equal(t, claims.Blob, full.Blob)
}

// TestExplain_SystemFileSuperseded and the deleted case: a fact's file ref
// stays pinned at the fact's commit after the file changes — the walk shows
// the OLD blob with superseded:true, and explaining at that commit returns
// the old content; after the file is removed, deleted:true.
// Sabotage: pin the file node at the branch tip instead of the fact's commit
// → the blob moves with the file and superseded stays false → red.
func TestExplain_SystemFileSuperseded(t *testing.T) {
	ri, ctx := sysFileRepo(t)
	factCommit := writeExplainFact(t, ctx, ri, "kb/reference/state.md", "state file fact", 0.9, []string{sysState})
	before := asFileNode(t, explainWalk(t, ctx, "kb/reference/state.md")[sysState])
	require.False(t, before.Superseded)

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.RawWriteForTest(context.Background(), "agent/test", sysState, "v2\n", "people change the system through git")
		require.NoError(t, err)
	}))

	after := asFileNode(t, explainWalk(t, ctx, "kb/reference/state.md")[sysState])
	require.Equal(t, factCommit, after.Commit)
	require.Equal(t, before.Blob, after.Blob, "the pin did not move")
	require.True(t, after.Superseded)
	require.False(t, after.Deleted)

	old := explainFileDirect(t, ctx, sysState, factCommit)
	require.Equal(t, "v1\n", *old.Content)
	require.True(t, old.Superseded, "the version asked for is not the tip's")
	require.Equal(t, "v2\n", *explainFileDirect(t, ctx, sysState, "").Content)

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().DeleteFact(context.Background(), "agent/test", sysState, "removed through git")
		require.NoError(t, err)
	}))
	gone := asFileNode(t, explainWalk(t, ctx, "kb/reference/state.md")[sysState])
	require.True(t, gone.Deleted)
	require.False(t, gone.Superseded)
	require.Equal(t, "v1\n", *explainFileDirect(t, ctx, sysState, factCommit).Content, "history still reads")
}

// TestExplain_LensQualifiedSystemFile: through a lens whose READ mount holds
// the file, kb://<id>/.knomit/… explains the file from that mount, and a
// fact there that refs it walks to a qualified file node. The read mount is
// pinned at its own branch, as a subscription mount is.
// Sabotage: resolve system paths against b.Write() only → "not found" → red.
func TestExplain_LensQualifiedSystemFile(t *testing.T) {
	repoA, _ := fedRepo(t)
	repoB, ctxB := sysFileRepo(t)
	writeExplainFact(t, ctxB, repoB, "kb/reference/skill.md", "skill file fact", 0.9, []string{sysSkill})
	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	ctx := repos.WithBinding(context.Background(), b)
	id := federate.ID12(repoB.ID())

	q := federate.QualifyPath(id, sysSkill)
	n := explainFileDirect(t, ctx, q, "")
	require.Equal(t, q, n.Path)
	require.Equal(t, sysFiles[sysSkill], *n.Content)

	// The same bare path is the WRITE repo, which has no such file.
	text, isErr := callExplain(t, ctx, map[string]any{"file": sysSkill})
	require.True(t, isErr, text)
	require.Contains(t, text, "not found")

	nodes := explainWalk(t, ctx, federate.QualifyPath(id, "kb/reference/skill.md"))
	fn := asFileNode(t, nodes[q])
	require.Equal(t, 1, fn.Depth)
	require.True(t, fn.Summary)
}

// TestExplain_OtherDotPathsStillClosed: only .knomit/ opens, and only by a
// well-formed exact path.
func TestExplain_OtherDotPathsStillClosed(t *testing.T) {
	_, ctx := sysFileRepo(t)
	for _, p := range []string{".github/x", "kb/.drafts/x.md", ".knomit/runs/../runs/state.txt", ".knomit//ontology.yaml", ".KNOMIT/ontology.yaml"} {
		text, isErr := callExplain(t, ctx, map[string]any{"file": p})
		require.Truef(t, isErr, "explain %s: %s", p, text)
		require.Containsf(t, text, "closed to the fact tools", "explain %s", p)
	}
}

func learnSysRefs(t *testing.T, ctx context.Context, refs []any) *mcpgo.CallToolResult {
	t.Helper()
	return callTool(t, LearnHandler(), ctx, map[string]any{
		"moment_name": "m",
		"facts": []any{map[string]any{"topic": "architecture", "category": "templates/files", "title": "A file fact about the claims trigger",
			"body": "Lands at .knomit/triggers/claims.js.", "confidence": 0.8, "sources": 1, "refs": refs}},
	})
}

// TestLearn_RefToMissingSystemFileRefused: a learn whose refs name a .knomit/
// file that is not at the tip is refused whole — every bad ref named, the fix
// line given, nothing committed; with the file present it lands, stored in
// the canonical kb://<own-id>/.knomit/<path> form, case kept. knomit_update
// adding a missing file ref is refused the same way.
// Sabotage: build learn's gate without WithFiles → "cannot check refs" (a
// different refusal) on the good call → red.
func TestLearn_RefToMissingSystemFileRefused(t *testing.T) {
	ri, ctx := sysFileRepo(t)
	before := headOf(t, ri, "agent/test")

	r := learnSysRefs(t, ctx, []any{".knomit/nope.js", ".knomit/skills/program-knomit/skill.md", sysClaims})
	require.True(t, r.IsError)
	text := resultText(t, r)
	require.Contains(t, text, "cites .knomit/nope.js, which does not exist")
	require.Contains(t, text, "cites .knomit/skills/program-knomit/skill.md, which does not exist", "case is significant")
	require.NotContains(t, text, "cites "+sysClaims)
	require.Contains(t, text, "does not exist under .knomit/ at the tip of the branch")
	require.Equal(t, before, headOf(t, ri, "agent/test"), "nothing was written")

	r = learnSysRefs(t, ctx, []any{sysClaims, sysSkill})
	require.False(t, r.IsError, resultText(t, r))
	var out struct {
		Commits []struct {
			File string `json:"file"`
		} `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, r)), &out), resultText(t, r))
	require.NotEmpty(t, out.Commits)
	path := out.Commits[0].File
	id := fact.ID12(ri.ID())
	var stored fact.Fact
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		res, err := svc.Facts().ReadFact(context.Background(), "agent/test", path, nil)
		require.NoError(t, err)
		stored, err = fact.ParseFact(path, res.Content)
		require.NoError(t, err)
	}))
	require.ElementsMatch(t, []string{"kb://" + id + "/" + sysClaims, "kb://" + id + "/" + sysSkill}, stored.Refs)

	r = callTool(t, UpdateHandler(), ctx, map[string]any{
		"file": path, "moment_name": "m",
		"updates": map[string]any{"refs": []any{sysClaims, ".knomit/gone.yaml"}},
	})
	require.True(t, r.IsError)
	require.Contains(t, resultText(t, r), "cites .knomit/gone.yaml, which does not exist")
}

// TestLearn_ArtifactWithSystemFileRef: agents' writes to the root artifacts/
// keep working (user ruling: "writes MUST WORK for artifacts in the root
// still"), including with a ref to a .knomit/ file; .knomit/ itself stays
// closed to the same door.
func TestLearn_ArtifactWithSystemFileRef(t *testing.T) {
	ri, ctx := sysFileRepo(t)
	r := callTool(t, LearnHandler(), ctx, map[string]any{
		"moment_name": "job",
		"facts": []any{map[string]any{"path": "artifacts/runs/state.md", "title": "Run state", "body": "cursor 1",
			"confidence": 0.9, "sources": 1, "refs": []any{sysOntology}}},
	})
	require.False(t, r.IsError, resultText(t, r))
	r = callTool(t, UpdateHandler(), ctx, map[string]any{
		"file": "artifacts/runs/state.md", "moment_name": "job", "updates": map[string]any{"body": "cursor 2"},
	})
	require.False(t, r.IsError, resultText(t, r))
	require.Equal(t, "cursor 2", artifactBodyOn(t, ri, "artifacts/runs/state.md"))

	r = learnAtPath(t, ctx, ".knomit/runs/x.md", "t", "b")
	require.True(t, r.IsError)
	require.Contains(t, resultText(t, r), "closed to the fact tools")
}

func artifactBodyOn(t *testing.T, ri *repos.RepoInstance, path string) string {
	t.Helper()
	var body string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		res, err := svc.Facts().ReadFact(context.Background(), "agent/test", path, nil)
		require.NoError(t, err)
		f, err := fact.ParseFact(path, res.Content)
		require.NoError(t, err)
		body = strings.TrimSpace(f.Body)
	}))
	return body
}

// TestQuery_PrivatePathStillRefused: reads by path opened; LISTING did not.
// knomit_query by a .knomit/ path is refused exactly as before.
func TestQuery_PrivatePathStillRefused(t *testing.T) {
	_, ctx := sysFileRepo(t)
	for _, p := range []string{".knomit/", ".knomit/skills/", sysSkill} {
		r := callTool(t, QueryHandler(), ctx, map[string]any{"path": p})
		require.Truef(t, r.IsError, "query path %s", p)
		require.Containsf(t, resultText(t, r), "is private", "query path %s", p)
	}
}
