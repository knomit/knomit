package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// ---- F08 PR A, M1: the F04 atomic move (`retract` on knomit_learn).
//
// The fixture is the learn_dedup fixture (newDedupAttrRepo): `inbox` is
// learn_dedup: off, so the task and the working copies never merge; `notes`
// is the dedup-ON control, where the length embedder makes identical text a
// near-duplicate at cosine 1.0.

// seedTask learns one templated fact under topic and returns its path.
func seedTask(t *testing.T, ctx context.Context, emb store.BatchEmbedder, topic string) string {
	t.Helper()
	r, err := LearnHandler(emb)(ctx, taskFactReq(topic))
	require.NoError(t, err)
	require.False(t, r.IsError, resultText(t, r))
	return mergedFactPath(t, r)
}

// moveReq builds a knomit_learn call writing facts and retracting retract.
func moveReq(moment string, facts []any, retract ...string) mcpgo.CallToolRequest {
	var req mcpgo.CallToolRequest
	rs := make([]any, len(retract))
	for i, p := range retract {
		rs[i] = p
	}
	req.Params.Arguments = map[string]any{"moment_name": moment, "facts": facts, "retract": rs}
	return req
}

// workingCopy is the fact a taker writes: its own queue, not the task's.
func workingCopy(title string, refs ...string) map[string]any {
	rs := []any{}
	for _, r := range refs {
		rs = append(rs, r)
	}
	return map[string]any{
		"topic": "inbox", "category": "me/working", "title": title,
		"body": "Taken: " + title, "type": "observation", "confidence": 0.8, "sources": 1,
		"entities": []any{"task-1"}, "refs": rs,
	}
}

// commitDiff is the name-status of commit h against its only parent.
func commitDiff(t *testing.T, svc *store.Service, h string) (store.CommitInfo, map[string]string) {
	t.Helper()
	ctx := context.Background()
	info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(h))
	require.NoError(t, err)
	require.Len(t, info.Parents, 1)
	rows, err := svc.Triggers().DiffFacts(ctx, info.Parents[0], plumbing.NewHash(h))
	require.NoError(t, err)
	out := map[string]string{}
	for _, r := range rows {
		out[r.Path] = r.Change
	}
	return info, out
}

func factExists(t *testing.T, svc *store.Service, path string) bool {
	t.Helper()
	ok, err := svc.Facts().FactExists(context.Background(), "agent/test", path)
	require.NoError(t, err)
	return ok
}

// underPrefix lists the live fact paths under prefix on the agent branch.
func underPrefix(t *testing.T, svc *store.Service, prefix string) []string {
	t.Helper()
	all, err := svc.Facts().ListAll(context.Background(), "agent/test")
	require.NoError(t, err)
	var out []string
	for _, p := range all {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// T-A1 MoveIsOneCommit: learn {facts:[w], retract:[t]} advances the branch by
// EXACTLY one commit whose name-status is `A w`, `D t`; the author is
// <id>+move@…, the message `move: <moment>`, and the response names the
// operation and the retracted path. Sabotage: write and retract with two
// BatchWrite calls → two commits → red.
func TestLearn_MoveIsOneCommit(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	svc := testRepoService(t, ctx)
	task := seedTask(t, ctx, emb, "inbox")
	before := headOf(t, ri, "agent/test")

	r, err := LearnHandler(emb)(ctx, moveReq("take task-1", []any{workingCopy("Working on task 1", task)}, task))
	require.NoError(t, err)
	require.False(t, r.IsError, resultText(t, r))
	w := mergedFactPath(t, r)

	after := headOf(t, ri, "agent/test")
	info, diff := commitDiff(t, svc, after)
	require.Equal(t, plumbing.NewHash(before), info.Parents[0], "exactly one commit on top of the pre-move head")
	require.Equal(t, map[string]string{w: "added", task: "deleted"}, diff, "one commit carries both halves")
	require.Contains(t, info.AuthorEmail, "+move@agents.knomit.io")
	require.True(t, strings.HasPrefix(info.Message, "move: take task-1"), info.Message)

	var out struct {
		Operation string   `json:"operation"`
		Retracted []string `json:"retracted"`
		Commit    string   `json:"commit"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, r)), &out))
	require.Equal(t, "move", out.Operation)
	require.Equal(t, []string{task}, out.Retracted)
	require.Equal(t, after, out.Commit)

	// facts: [] with a retract is a batch retraction in one commit.
	other := seedTask(t, ctx, emb, "inbox")
	mid := headOf(t, ri, "agent/test")
	r, err = LearnHandler(emb)(ctx, moveReq("drop", []any{}, other))
	require.NoError(t, err)
	require.False(t, r.IsError, resultText(t, r))
	info, diff = commitDiff(t, svc, headOf(t, ri, "agent/test"))
	require.Equal(t, plumbing.NewHash(mid), info.Parents[0])
	require.Equal(t, map[string]string{other: "deleted"}, diff)
	require.Contains(t, info.AuthorEmail, "+retract@agents.knomit.io")
	require.True(t, strings.HasPrefix(info.Message, "retract: drop"), info.Message)

	// And neither half: still refused.
	r, err = LearnHandler(emb)(ctx, moveReq("nothing", []any{}))
	require.NoError(t, err)
	require.True(t, r.IsError)
	require.Contains(t, resultText(t, r), "facts must not be empty")
}

// T-A2 RetractMissingRefusesAll: retracting an ABSENT leaf in a directory that
// exists (the probe's silent-commit shape) refuses the WHOLE call: the error
// names the path, HEAD does not move, and the fact the call would have
// written does not exist. Sabotage: drop mustExist (plain BatchWriteFacts) →
// the absent delete is a silent no-op and the write commits → red.
func TestLearn_RetractMissingRefusesAll(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	svc := testRepoService(t, ctx)
	task := seedTask(t, ctx, emb, "inbox")
	absent := task[:strings.LastIndex(task, "/")+1] + "00000000-gone.md"
	before := headOf(t, ri, "agent/test")

	r, err := LearnHandler(emb)(ctx, moveReq("take", []any{workingCopy("Working on a gone task")}, task, absent))
	require.NoError(t, err)
	require.True(t, r.IsError, "an absent retract path refuses the call")
	require.Contains(t, resultText(t, r), absent)
	require.Contains(t, resultText(t, r), "nothing written")
	require.NotContains(t, resultText(t, r), task+",", "only the absent path is named")
	require.Equal(t, before, headOf(t, ri, "agent/test"), "HEAD unchanged")
	require.Empty(t, underPrefix(t, svc, "kb/inbox/me/working/"), "the working copy was not written")
	require.True(t, factExists(t, svc, task), "the present path was not retracted either")
}

// T-A3 ConcurrentMovesOneWins: ten goroutines move the SAME task on one
// branch. Exactly one succeeds; the other nine are refused because the task
// is gone; exactly one working copy exists. The store's pre-lock hook holds
// every caller at a barrier until all ten have passed their pre-flight, which
// is the window an existence check OUTSIDE the lock would go stale in.
// Sabotage: check existence before the lock (as knomit_retract does) → all
// ten see the task, all ten commit → red, deterministically.
func TestLearn_ConcurrentMovesOneWins(t *testing.T) {
	_, ctx, emb := newDedupAttrRepo(t)
	svc := testRepoService(t, ctx)
	task := seedTask(t, ctx, emb, "inbox")

	const n = 10
	var arrived sync.WaitGroup
	arrived.Add(n)
	release := make(chan struct{})
	var once sync.Once
	restore := store.SetBatchWriteUnlockedHookForTest(func() {
		arrived.Done()
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	})
	defer restore()
	go func() {
		arrived.Wait()
		once.Do(func() { close(release) })
	}()

	var wg sync.WaitGroup
	results := make([]*mcpgo.CallToolResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = LearnHandler(emb)(ctx, moveReq(fmt.Sprintf("take %d", i),
				[]any{workingCopy(fmt.Sprintf("Working copy %d", i))}, task))
		}(i)
	}
	wg.Wait()
	once.Do(func() { close(release) })

	ok, refused := 0, 0
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		if results[i].IsError {
			require.Contains(t, resultText(t, results[i]), "not on this branch: "+task)
			refused++
		} else {
			ok++
		}
	}
	require.Equal(t, 1, ok, "exactly one move wins")
	require.Equal(t, n-1, refused)
	require.Len(t, underPrefix(t, svc, "kb/inbox/me/working/"), 1, "exactly one working copy")
	require.False(t, factExists(t, svc, task))
}

// T-A4 WriteAndRetractSamePathRefused: a call that writes the path it also
// retracts is refused by name before any work; nothing is written. (The
// store would otherwise apply the delete last and lose the write silently.)
// Sabotage: remove the check → the call fails later for another reason (the
// explicit slot already exists), or not at all → the message check is red.
func TestLearn_WriteAndRetractSamePathRefused(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	const slot = ".knomit/jobs/state.md"
	job := map[string]any{"path": slot, "title": "Job state", "body": "v1"}
	r, err := LearnHandler(emb)(ctx, moveReq("seed", []any{job}))
	require.NoError(t, err)
	require.False(t, r.IsError, resultText(t, r))
	before := headOf(t, ri, "agent/test")

	job2 := map[string]any{"path": slot, "title": "Job state", "body": "v2"}
	r, err = LearnHandler(emb)(ctx, moveReq("both", []any{job2}, slot))
	require.NoError(t, err)
	require.True(t, r.IsError)
	require.Contains(t, resultText(t, r), "both written and retracted")
	require.Equal(t, before, headOf(t, ri, "agent/test"), "nothing written")
}

// T-A5 DedupNeverFoldsIntoRetracted: in a dedup-ON topic an incoming fact that
// near-duplicates t (cosine 1.0) while the same call retracts t is written at
// a NEW path, and t is gone. The dedup search DID score t — the skip hook
// records it — so the exclusion is exercised, not bypassed; the control call
// without retract merges into t. Sabotage: drop the exclusion → folded into t
// → the call is refused (or the new fact is lost) → red.
func TestLearn_DedupNeverFoldsIntoRetracted(t *testing.T) {
	_, ctx, emb := newDedupAttrRepo(t)
	svc := testRepoService(t, ctx)
	target := seedTask(t, ctx, emb, "notes")

	// Control: the same text without retract merges into the target.
	r, err := LearnHandler(emb)(ctx, taskFactReq("notes"))
	require.NoError(t, err)
	require.False(t, r.IsError, resultText(t, r))
	require.Equal(t, target, mergedFactPath(t, r), "control: this text dedup-merges into the target")

	var skipped []string
	onDedupSkipRetracted = func(p string) { skipped = append(skipped, p) }
	t.Cleanup(func() { onDedupSkipRetracted = nil })

	req := taskFactReq("notes")
	req.Params.Arguments.(map[string]any)["retract"] = []any{target}
	r, err = LearnHandler(emb)(ctx, req)
	require.NoError(t, err)
	require.False(t, r.IsError, resultText(t, r))
	written := mergedFactPath(t, r)
	require.Equal(t, []string{target}, skipped, "dedup scored the retracted fact and declined it")
	require.NotEqual(t, target, written, "written at a NEW path")
	require.True(t, factExists(t, svc, written))
	require.False(t, factExists(t, svc, target), "the retracted fact is gone")
}

// T-A6 RefToRetractedPathPasses: the new fact refs t while the same call
// retracts t; the ref gate judges the PRE-write head, where t is live, so the
// call succeeds. Sabotage: gate against the post-write tree → refused → red.
func TestLearn_RefToRetractedPathPasses(t *testing.T) {
	_, ctx, emb := newDedupAttrRepo(t)
	svc := testRepoService(t, ctx)
	task := seedTask(t, ctx, emb, "inbox")

	r, err := LearnHandler(emb)(ctx, moveReq("take", []any{workingCopy("Working, citing the task", task)}, task))
	require.NoError(t, err)
	require.False(t, r.IsError, resultText(t, r))
	w := mergedFactPath(t, r)
	res, err := svc.Facts().ReadFact(context.Background(), "agent/test", w, nil)
	require.NoError(t, err)
	require.Contains(t, res.Content, strings.TrimPrefix(task, "kb/"), "the ref to the retracted task is kept")
	require.False(t, factExists(t, svc, task))
}

// T-A7 (MCP half) RetractPrivateRefused: a non-writable private path (a
// `.drafts` fact) is refused exactly as knomit_retract refuses it, nothing
// written. The script-host half is TestScript_LearnRetractPrivateRefused.
// Sabotage: skip the private rule in learnRetractPaths → red.
func TestLearn_RetractPrivateRefused(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	before := headOf(t, ri, "agent/test")
	r, err := LearnHandler(emb)(ctx, moveReq("sneak", []any{workingCopy("x")}, "kb/.drafts/secret.md"))
	require.NoError(t, err)
	require.True(t, r.IsError)
	require.Contains(t, resultText(t, r), "is private")
	require.Equal(t, before, headOf(t, ri, "agent/test"))
}
