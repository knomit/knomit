package web

// #349 end to end: the agent's `trace` argument over the real router, the MCP
// transport, knomit_bind and the real handlers — what a session started by a
// recipe actually does.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

var (
	e2eCause = strings.Repeat("e", 40)
	e2eRun   = "run-" + strings.Repeat("f", 32)
)

// commitMessageIn reads one commit's message from a repo's store.
func commitMessageIn(t *testing.T, m *repos.Manager, repo, hash string) string {
	t.Helper()
	var msg string
	require.NoError(t, m.Get(repo).WithRead(func(svc *store.Service) {
		info, err := svc.Triggers().CommitInfo(context.Background(), plumbing.NewHash(hash))
		require.NoError(t, err)
		msg = info.Message
	}))
	return msg
}

func traceJSON(trace map[string]string) string {
	if trace == nil {
		return ""
	}
	b, _ := json.Marshal(trace)
	return `,"trace":` + string(b)
}

// learnTraced writes one fact through the handle and returns its file and
// commit hash.
func learnTraced(t *testing.T, call func(name, args string) (string, bool), handle, title string, trace map[string]string, extra string) (string, string) {
	t.Helper()
	text, isErr := call("knomit_learn", fmt.Sprintf(
		`{"binding":%q,"moment_name":"session work","facts":[{"topic":"architecture","category":"trace/e2e","title":%q,"body":"written by a traced session","entities":[%q]}]%s%s}`,
		handle, title, title, extra, traceJSON(trace)))
	require.False(t, isErr, text)
	var res struct {
		Commits []struct {
			File string `json:"file"`
			Hash string `json:"hash"`
		} `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &res), text)
	require.Len(t, res.Commits, 1, text)
	return res.Commits[0].File, res.Commits[0].Hash
}

// T1 + T2: a trace passed to knomit_learn, knomit_update and the
// knomit_retract TOOL ends each commit with exactly its paragraph (Trace,
// Cause, Run, then the agent's own keys); learn's atomic move (retract
// option) carries it on its one commit; the same calls WITHOUT a trace carry
// no paragraph and every key reads "" (D-mint). Sabotage: drop applyTrace in
// one handler (T1 red); make an absent trace yield a fixed set (T2 red).
func TestAgentTrace_E2E_StampsEachWriteAndNothingWithout(t *testing.T) {
	h, m := experimentServer(t)
	sid := initExperimentSession(t, h)
	handle := bindHandle(t, h, sid, "jobA-repo")
	call := func(name, args string) (string, bool) { return callToolAt(t, h, unscopedMount, sid, name, args) }
	trace := map[string]string{"Knomit-Trace": "task-7f3a", "Knomit-Cause": e2eCause, "Knomit-Run": e2eRun, "Ticket": "ABC-12"}
	want := "\n\nKnomit-Trace: task-7f3a\nKnomit-Cause: " + e2eCause + "\nKnomit-Run: " + e2eRun + "\nTicket: ABC-12\n"

	for _, traced := range []bool{true, false} {
		var tr map[string]string
		if traced {
			tr = trace
		}
		suffix := fmt.Sprintf("%v", traced)
		file, hash := learnTraced(t, call, handle, "Fact one "+suffix, tr, "")
		learnMsg := commitMessageIn(t, m, "jobA-repo", hash)

		text, isErr := call("knomit_update", fmt.Sprintf(`{"binding":%q,"file":%q,"moment_name":"session work","updates":{"confidence":0.9}%s}`,
			handle, file, traceJSON(tr)))
		require.False(t, isErr, text)
		var up struct {
			Commit string `json:"commit"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &up), text)
		updateMsg := commitMessageIn(t, m, "jobA-repo", up.Commit)

		// The atomic move: a new fact and the old one's removal, one commit.
		file2, moveHash := learnTraced(t, call, handle, "Fact two "+suffix, tr, fmt.Sprintf(`,"retract":[%q]`, file))
		moveMsg := commitMessageIn(t, m, "jobA-repo", moveHash)

		text, isErr = call("knomit_retract", fmt.Sprintf(`{"binding":%q,"file":%q,"moment_name":"session work"%s}`, handle, file2, traceJSON(tr)))
		require.False(t, isErr, text)
		var rt struct {
			Commit string `json:"commit"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &rt), text)
		retractMsg := commitMessageIn(t, m, "jobA-repo", rt.Commit)

		// The EXACT message each tool writes on its own. Untraced, that is the
		// whole message — no paragraph of any key, Knomit- or not (D-mint);
		// traced, it is followed by exactly the trace paragraph.
		bare := map[string]string{
			"learn":   "learn: session work",
			"update":  "update(session work): Fact one " + suffix,
			"move":    "move: session work",
			"retract": "retract(session work): " + file2,
		}
		for name, msg := range map[string]string{"learn": learnMsg, "update": updateMsg, "move": moveMsg, "retract": retractMsg} {
			if traced {
				require.Equal(t, bare[name]+want, msg, "%s: the tool's message plus exactly the trace paragraph", name)
				require.Equal(t, "task-7f3a", store.TrailerValue(msg, store.TrailerTrace), name)
				require.Equal(t, e2eRun, store.TrailerValue(msg, store.TrailerRun), name)
			} else {
				require.Equal(t, bare[name], msg, "%s: D-mint — no trace passed, no paragraph of any kind", name)
			}
		}
	}
}

// T3: one session, two tasks side by side. Two goroutines write through ONE
// MCP session and ONE handle at the same time, each with its own trace; every
// commit carries the trace of the call that made it — the server keeps no
// trace between calls. Sabotage: cache the last trace per MCP session or per
// binding (commits pick up the other task's trace).
func TestAgentTrace_E2E_ParallelTasksOnOneSession(t *testing.T) {
	h, m := experimentServer(t)
	sid := initExperimentSession(t, h)
	handle := bindHandle(t, h, sid, "jobA-repo")
	call := func(name, args string) (string, bool) { return callToolAt(t, h, unscopedMount, sid, name, args) }

	const perTask = 6
	type written struct{ task, hash string }
	var mu sync.Mutex
	var got []written
	var wg sync.WaitGroup
	for _, task := range []string{"task-a", "task-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perTask; i++ {
				// The untraced task-b writes alternate with traced ones, so a
				// cached trace would also show up where none was passed.
				var tr map[string]string
				if task == "task-a" || i%2 == 0 {
					tr = map[string]string{"Knomit-Trace": task, "Step": fmt.Sprintf("%s-%d", task, i)}
				}
				_, hash := learnTraced(t, call, handle, fmt.Sprintf("%s write %d", task, i), tr, "")
				label := task
				if tr == nil {
					label = ""
				}
				mu.Lock()
				got = append(got, written{label, hash})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Len(t, got, 2*perTask)
	for _, w := range got {
		msg := commitMessageIn(t, m, "jobA-repo", w.hash)
		require.Equal(t, w.task, store.TrailerValue(msg, store.TrailerTrace), "%q", msg)
		if w.task == "" {
			require.NotContains(t, msg, "\n\n", "an untraced write carries no paragraph of any kind: %q", msg)
		} else {
			require.True(t, strings.HasPrefix(store.TrailerValue(msg, "Step"), w.task+"-"), "%q", msg)
		}
	}
}

// commitsSince lists the messages of the first-parent commits on branch
// after stop, oldest first.
func commitsSince(t *testing.T, svc *store.Service, branch, stop string) []string {
	t.Helper()
	ctx := context.Background()
	h, err := svc.Branches().HeadCommit(ctx, branch)
	require.NoError(t, err)
	var out []string
	for h != stop {
		info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(h))
		require.NoError(t, err)
		out = append([]string{info.Message}, out...)
		require.NotEmpty(t, info.Parents, "walked past the root looking for %s", stop)
		h = info.Parents[0].String()
	}
	return out
}
