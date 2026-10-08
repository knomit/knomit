package repos

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// ---- Fixture
//
// A real Manager (newTestManager: agent branch "agent/test", background sync
// off) with one repo. Triggers are installed by writing the ontology file on
// the agent branch, exactly as a user's commit (or a merge from a peer) would;
// the dispatcher reads it from the head, never from ri.Ontology().

const trigAgent = "agent/test"

// triggerOntology renders an ontology with the given trigger entries under
// `tasks` (each entry is one YAML list item, already indented by 6) and an
// `other` topic nothing fires on. rootAttrs is an optional attributes block.
func triggerOntology(rootAttrs string, entries ...string) string {
	var b strings.Builder
	b.WriteString("id: trig\nname: Triggers\n")
	if rootAttrs != "" {
		b.WriteString(rootAttrs)
	}
	b.WriteString("topics:\n  tasks:\n    description: tasks\n")
	if len(entries) > 0 {
		b.WriteString("    triggers:\n")
		for _, e := range entries {
			b.WriteString(e)
			if !strings.HasSuffix(e, "\n") {
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("  other:\n    description: other\n")
	return b.String()
}

// trig renders one trigger entry: name, on, optional match and if.
func trig(name, on, match, cond string) string {
	e := fmt.Sprintf("      - name: %s\n        on: %s\n        do: emit\n", name, on)
	if match != "" {
		e += fmt.Sprintf("        match: %q\n", match)
	}
	if cond != "" {
		e += fmt.Sprintf("        if: %q\n", cond)
	}
	return e
}

func factBody(title string) string {
	return "---\ntype: observation\nconfidence: 0.8\nsources: 1\n---\n# " + title + "\n\nbody\n"
}

// writeOn writes a fact on branch and returns the commit.
func writeOn(t *testing.T, ri *RepoInstance, branch, path string) string {
	t.Helper()
	r, err := testService(t, ri).Facts().WriteFact(context.Background(), branch, path, factBody(path), "learn: "+path, "learn")
	require.NoError(t, err)
	return r.CommitHash
}

// writeMsg is writeOn with an explicit commit message (for trailers).
func writeMsg(t *testing.T, ri *RepoInstance, branch, path, msg string) string {
	t.Helper()
	r, err := testService(t, ri).Facts().WriteFact(context.Background(), branch, path, factBody(path), msg, "learn")
	require.NoError(t, err)
	return r.CommitHash
}

func updateOn(t *testing.T, ri *RepoInstance, branch, path string) string {
	t.Helper()
	r, err := testService(t, ri).Facts().WriteFact(context.Background(), branch, path, factBody(path+" v2"), "update: "+path, "update")
	require.NoError(t, err)
	return r.CommitHash
}

func deleteOn(t *testing.T, ri *RepoInstance, branch, path string) string {
	t.Helper()
	h, err := testService(t, ri).Facts().DeleteFact(context.Background(), branch, path, "retract: "+path)
	require.NoError(t, err)
	return h
}

// setOntology commits yaml as the ontology on the agent branch and waits for
// the dispatcher to process that head.
func setOntology(t *testing.T, ri *RepoInstance, yaml string) string {
	t.Helper()
	r, err := testService(t, ri).Facts().WriteFact(context.Background(), trigAgent, OntologyPath, yaml, "ontology", "updated")
	require.NoError(t, err)
	waitTriggerHead(t, ri, r.CommitHash)
	return r.CommitHash
}

// waitTriggerHead waits until a dispatcher run that read head has completed.
func waitTriggerHead(t *testing.T, ri *RepoInstance, head string) {
	t.Helper()
	waitTriggerHeadFor(t, ri, head, 20*time.Second)
}

// waitTriggerHeadFor waits until a run that read head has completed AND its
// phase C has been flushed to the tables (the flush waits for a short quiet
// period after the run).
func waitTriggerHeadFor(t *testing.T, ri *RepoInstance, head string, timeout time.Duration) {
	t.Helper()
	require.NotNil(t, ri.triggers, "the repo must have a dispatcher")
	require.Eventually(t, func() bool {
		got, _ := ri.triggers.runSequence()
		return got == head && ri.triggers.flushed()
	}, timeout, 10*time.Millisecond, "the dispatcher never completed and flushed a run at %s", head)
}

// write writes a fact on the agent branch and waits for its run.
func write(t *testing.T, ri *RepoInstance, path string) string {
	t.Helper()
	h := writeOn(t, ri, trigAgent, path)
	waitTriggerHead(t, ri, h)
	return h
}

func head(t *testing.T, ri *RepoInstance) string {
	t.Helper()
	h, err := testService(t, ri).Branches().HeadCommit(context.Background(), trigAgent)
	require.NoError(t, err)
	return h
}

func fires(t *testing.T, ri *RepoInstance) []store.TriggerFire {
	t.Helper()
	rows, err := testService(t, ri).Triggers().RecentTriggerFires(context.Background(), trigAgent, 20000)
	require.NoError(t, err)
	return rows
}

// firesOf is the fire rows of one trigger (run rows excluded), newest first.
func firesOf(t *testing.T, ri *RepoInstance, name string) []store.TriggerFire {
	t.Helper()
	var out []store.TriggerFire
	for _, f := range fires(t, ri) {
		if f.Trigger == name {
			out = append(out, f)
		}
	}
	return out
}

func runRows(t *testing.T, ri *RepoInstance) []store.TriggerFire {
	t.Helper()
	var out []store.TriggerFire
	for _, f := range fires(t, ri) {
		if f.Outcome == store.TriggerOutcomeRun {
			out = append(out, f)
		}
	}
	return out
}

func watermarks(t *testing.T, ri *RepoInstance) map[string]string {
	t.Helper()
	wms, err := testService(t, ri).Triggers().TriggerWatermarks(context.Background(), trigAgent)
	require.NoError(t, err)
	return wms
}

func pathsOf(rows []store.TriggerFire) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Path)
	}
	return out
}

// setHooks installs test hooks for the test's duration.
func setHooks(t *testing.T, h triggerHooks) {
	t.Helper()
	triggerHooksMu.Lock()
	triggerTestHooks = h
	triggerHooksMu.Unlock()
	t.Cleanup(func() {
		triggerHooksMu.Lock()
		triggerTestHooks = triggerHooks{}
		triggerHooksMu.Unlock()
	})
}

// parkDispatcher blocks the worker between the kick and the ref read until
// release is called. Only the FIRST run is parked. The hooks already
// installed (a clock, a slow `if`) stay in force while parked and after.
func parkDispatcher(t *testing.T, ri *RepoInstance) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	prev := currentTriggerHooks()
	parked := prev
	parked.afterKick = func() {
		<-gate
	}
	setHooks(t, parked)
	return func() {
		once.Do(func() {
			close(gate)
			setHooks(t, prev)
		})
	}
}

// slowIf makes the named trigger's own work take ms of REAL wall time on the
// paths match accepts (every path when match is nil), keeping the other
// hooks installed. The sandbox's Date.now() is the run's pinned clock, so an
// `if` cannot spin on the wall clock, and a spin counted in iterations
// shrinks under CPU contention; a sleep inside the timed section does not.
func slowIf(t *testing.T, trigger string, ms int, match func(path string) bool) {
	t.Helper()
	h := currentTriggerHooks()
	h.evalDelay = func(name, path string) time.Duration {
		if name == trigger && (match == nil || match(path)) {
			return time.Duration(ms) * time.Millisecond
		}
		return 0
	}
	setHooks(t, h)
}

// newTriggerRepo boots a repo with the given trigger entries installed.
func newTriggerRepo(t *testing.T, entries ...string) (*Manager, *RepoInstance) {
	t.Helper()
	m := newTestManager(t)
	ri := bootRepo(t, m)
	setOntology(t, ri, triggerOntology("", entries...))
	return m, ri
}

// captureLogs routes the global logger into a buffer for the test.
func captureLogs(t *testing.T, level zerolog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf).Level(level)
	t.Cleanup(func() { log.Logger = orig })
	return &buf
}

func countLines(buf *bytes.Buffer, needle string) int {
	return strings.Count(buf.String(), needle)
}

// ---- Placeholders, matching, episodes

// InboxPlaceholder: `tasks/*/inbox/{agent}/*.md` fires for this agent's inbox
// and not for another's. The agent id is the branch minus "agent/".
// Sabotage: substitute {agent} with `*`.
func TestDispatch_InboxPlaceholder(t *testing.T) {
	t.Parallel()
	_, ri := newTriggerRepo(t, trig("my-inbox", "[learn, update]", "tasks/*/inbox/{agent}/*.md", ""))
	write(t, ri, "kb/tasks/p1/inbox/test/a.md")
	write(t, ri, "kb/tasks/p1/inbox/other-host-12345678/b.md")
	got := firesOf(t, ri, "my-inbox")
	require.Equal(t, []string{"kb/tasks/p1/inbox/test/a.md"}, pathsOf(got))
	require.Equal(t, "learn", got[0].Episode)
	require.Equal(t, "local", got[0].Source)
}

// MergeAddModifyDeleteOncePerPath: a merge bringing three commits that add A,
// modify B and delete C fires exactly A learn, B update, C retract, each with
// the ORIGINAL commit; re-running the merge (a no-op) fires nothing.
// Sabotage: replay per commit (B fires twice), or use the merge as commit.
func TestDispatch_MergeAddModifyDeleteOncePerPath(t *testing.T) {
	t.Parallel()
	_, ri := newTriggerRepo(t, trig("all", "[learn, update, retract]", "", ""))
	ctx := context.Background()
	svc := testService(t, ri)
	write(t, ri, "kb/tasks/b.md")
	write(t, ri, "kb/tasks/c.md")
	require.NoError(t, svc.Branches().CreateBranch(ctx, "peer", trigAgent))
	write(t, ri, "kb/tasks/mine.md") // the agent's own work, so the merge is a real merge
	before := len(firesOf(t, ri, "all"))

	a := writeOn(t, ri, "peer", "kb/tasks/a.md")
	updateOn(t, ri, "peer", "kb/tasks/b.md")
	// A second, DIFFERENT version: the LAST toucher names the fire.
	r, err := testService(t, ri).Facts().WriteFact(ctx, "peer", "kb/tasks/b.md", factBody("b v3"), "update: b again", "update")
	require.NoError(t, err)
	b := r.CommitHash
	c := deleteOn(t, ri, "peer", "kb/tasks/c.md")
	require.NoError(t, svc.Branches().MergeBranch(ctx, "peer", trigAgent, store.StrategyRemoteWins))
	h := head(t, ri)
	waitTriggerHead(t, ri, h)

	got := firesOf(t, ri, "all")[:len(firesOf(t, ri, "all"))-before]
	byPath := map[string]store.TriggerFire{}
	for _, f := range got {
		byPath[f.Path] = f
	}
	require.Len(t, got, 3, "one fire per path per advance: %+v", pathsOf(got))
	require.Equal(t, "learn", byPath["kb/tasks/a.md"].Episode)
	require.Equal(t, a, byPath["kb/tasks/a.md"].Commit)
	require.Equal(t, "update", byPath["kb/tasks/b.md"].Episode)
	require.Equal(t, b, byPath["kb/tasks/b.md"].Commit, "the LAST non-merge toucher, not the first and not the merge")
	require.Equal(t, "retract", byPath["kb/tasks/c.md"].Episode)
	require.Equal(t, c, byPath["kb/tasks/c.md"].Commit)
	for _, f := range got {
		require.NotEqual(t, h, f.Commit, "never this machine's merge commit")
		require.Equal(t, h, f.RangeTo)
	}

	// The same merge again is a no-op advance: nothing fires.
	require.NoError(t, svc.Branches().MergeBranch(ctx, "peer", trigAgent, store.StrategyRemoteWins))
	require.Equal(t, h, head(t, ri), "fixture: the re-merge is a no-op")
	time.Sleep(150 * time.Millisecond)
	require.Len(t, firesOf(t, ri, "all"), before+3)
}

// PrefixChangeNotInTip: an advance with h1 (a tasks change) and h2 (an other
// change) fires for h1's path with change.commit = h1. Sabotage: use the tip.
func TestDispatch_PrefixChangeNotInTip(t *testing.T) {
	_, ri := newTriggerRepo(t, trig("tasks", "learn", "", ""))
	release := parkDispatcher(t, ri)
	h1 := writeOn(t, ri, trigAgent, "kb/tasks/t.md")
	h2 := writeOn(t, ri, trigAgent, "kb/other/o.md")
	release()
	waitTriggerHead(t, ri, h2)
	got := firesOf(t, ri, "tasks")
	require.Len(t, got, 1)
	require.Equal(t, "kb/tasks/t.md", got[0].Path)
	require.Equal(t, h1, got[0].Commit)
}

// SourceLocalVsMerged: an own write is `local`; a peer's write merged in is
// `merged`; an experiment committed into the agent branch is `local` (this
// instance's key signed it, whatever the author text says). Sabotage:
// classify by author text — the experiment reads `merged`.
func TestDispatch_SourceLocalVsMerged(t *testing.T) {
	t.Parallel()
	_, ri := newTriggerRepo(t, trig("all", "[learn, update]", "", ""))
	ctx := context.Background()
	svc := testService(t, ri)

	write(t, ri, "kb/tasks/own.md")

	// A peer's commit on main, signed with ANOTHER key, merged into the agent
	// branch.
	svc.SetSigner(testsigner.Named("peer"))
	writeOn(t, ri, "main", "kb/tasks/peer.md")
	svc.SetSigner(nil) // back to the test fallback, this instance's key
	require.NoError(t, svc.Branches().MergeBranch(ctx, "main", trigAgent, store.StrategyRemoteWins))
	waitTriggerHead(t, ri, head(t, ri))

	// An experiment: authored exp/<name> (no fingerprint in the author), but
	// signed by this instance.
	_, err := svc.Experiments().OpenExperiment(ctx, "e1", "", trigAgent)
	require.NoError(t, err)
	writeOn(t, ri, "exp/e1", "kb/tasks/exp.md")
	_, err = svc.Experiments().CommitExperiment(ctx, "e1", nil)
	require.NoError(t, err)
	waitTriggerHead(t, ri, head(t, ri))

	src := map[string]string{}
	for _, f := range firesOf(t, ri, "all") {
		src[f.Path] = f.Source
	}
	require.Equal(t, map[string]string{
		"kb/tasks/own.md":  "local",
		"kb/tasks/peer.md": "merged",
		"kb/tasks/exp.md":  "local",
	}, src)
}

// TraceFromTrailer: a firing commit whose message carries `Knomit-Trace: t-1`
// gives change.trace = "t-1"; one without gives "".
func TestDispatch_TraceFromTrailer(t *testing.T) {
	t.Parallel()
	_, ri := newTriggerRepo(t,
		trig("traced", "learn", "", "change.trace === 't-1'"),
		trig("all", "learn", "", ""))
	h := writeMsg(t, ri, trigAgent, "kb/tasks/with.md", "learn: with\n\nKnomit-Trace: t-1\n")
	waitTriggerHead(t, ri, h)
	write(t, ri, "kb/tasks/without.md")
	require.Equal(t, []string{"kb/tasks/with.md"}, pathsOf(firesOf(t, ri, "traced")))
	all := firesOf(t, ri, "all")
	require.Len(t, all, 2)
	for _, f := range all {
		if f.Path == "kb/tasks/with.md" {
			require.Equal(t, "t-1", f.Trace)
		} else {
			// Since F07 PR 3 the trace is DERIVED and never empty: a commit
			// without a trailer (and a fact that is not a task) is its own
			// story, keyed by its hash.
			require.Equal(t, f.Commit, f.Trace, "a commit without a trailer is the root of its own story")
			require.Len(t, f.Trace, 40)
		}
	}
}

// ---- verified

const verifyLog = "attributes:\n  verify_signatures: log\n"

// AuthorVerifiedOnlyUnderF09: with verify_signatures absent, a validly signed
// commit reads verified:false; with `log` and the commit reachable from main
// (accepted at the gate, F09 PR 5), true; an own unmerged write above main,
// false. Sabotage: report signature-valid as verified.
func TestDispatch_AuthorVerifiedOnlyUnderF09(t *testing.T) {
	m := newTestManager(t)
	ri := bootRepo(t, m)
	svc := testService(t, ri)
	entries := []string{
		trig("verified-only", "learn", "", "change.author.verified === true"),
		trig("all", "learn", "", ""),
	}

	// Verification absent: a valid signature is a claim, not a verification.
	setOntology(t, ri, triggerOntology("", entries...))
	write(t, ri, "kb/tasks/off.md")
	require.Empty(t, firesOf(t, ri, "verified-only"))
	require.Len(t, firesOf(t, ri, "all"), 1)

	// verify_signatures: log, main moved to a commit (as the gate would): a
	// write BELOW main (parked so main covers it before the run) is verified;
	// the next write above it is not.
	setOntology(t, ri, triggerOntology(verifyLog, entries...))
	release := parkDispatcher(t, ri)
	below := writeOn(t, ri, trigAgent, "kb/tasks/below.md")
	require.NoError(t, svc.TestingSetRef("refs/heads/main", below))
	release()
	waitTriggerHead(t, ri, below)
	write(t, ri, "kb/tasks/above.md")
	require.Equal(t, []string{"kb/tasks/below.md"}, pathsOf(firesOf(t, ri, "verified-only")))
}

// VerifiedAfterRestart [R2-1]: no cache of main's history survives a restart;
// the dispatcher must walk it itself on the first run that needs it, not read
// every fire as unverified. Sabotage: treat an empty cache as "not below".
func TestDispatch_VerifiedAfterRestart(t *testing.T) {
	home := t.TempDir()
	deps := Deps{Cfg: config.Config{Home: home, OntologyRoot: "kb"}, AgentBranch: trigAgent,
		KeyPath: filepath.Join(home, "agent.key"), Machine: Options{Synchronous: true, CrashBackoff: testCrashBackoff}}
	m := New(context.Background(), deps)
	ri := bootRepo(t, m)
	entries := []string{trig("verified-only", "learn", "", "change.author.verified === true")}
	setOntology(t, ri, triggerOntology(verifyLog, entries...))
	write(t, ri, "kb/tasks/first.md")
	require.NoError(t, m.Close())

	// Restart: no fold has run in this process, so the store cache is nil.
	m2 := New(context.Background(), deps)
	require.NoError(t, m2.Start())
	t.Cleanup(func() { _ = m2.Close() })
	ri2 := m2.Get(testRepoName)
	require.NotNil(t, ri2)
	waitTriggerHead(t, ri2, head(t, ri2))
	release := parkDispatcher(t, ri2)
	c := writeOn(t, ri2, trigAgent, "kb/tasks/after-restart.md")
	require.NoError(t, testService(t, ri2).TestingSetRef("refs/heads/main", c))
	release()
	waitTriggerHead(t, ri2, c)
	require.Equal(t, []string{"kb/tasks/after-restart.md"}, pathsOf(firesOf(t, ri2, "verified-only")),
		"a commit reachable from main is verified even though no history cache survives the restart")
}

// ---- Watermarks

// FirstAppearanceNoBackfill: a trigger added in a commit that also adds a
// matching fact fires nothing for it; the next matching write fires.
// Sabotage: W = the root commit.
func TestDispatch_FirstAppearanceNoBackfill(t *testing.T) {
	t.Parallel()
	m := newTestManager(t)
	ri := bootRepo(t, m)
	svc := testService(t, ri)
	write := func(p string) string { return writeOn(t, ri, trigAgent, p) }
	before := write("kb/tasks/before.md")
	// One commit: the ontology with the trigger AND a matching fact.
	h, _, err := svc.Facts().BatchWriteFacts(context.Background(), trigAgent, map[string]string{
		OntologyPath:               triggerOntology("", trig("all", "learn", "", "")),
		"kb/tasks/with-trigger.md": factBody("with"),
	}, nil, "trigger + fact", "updated")
	require.NoError(t, err)
	waitTriggerHead(t, ri, h)
	require.Equal(t, h, watermarks(t, ri)["all"], "first appearance bookmarks the head of that advance")
	require.Empty(t, firesOf(t, ri, "all"), "neither the earlier fact (%s) nor the same-commit fact fires", before)
	after := write("kb/tasks/after.md")
	waitTriggerHead(t, ri, after)
	require.Equal(t, []string{"kb/tasks/after.md"}, pathsOf(firesOf(t, ri, "all")))
}

// RemovedTriggerForgets: a trigger whose NAME is removed then re-added does
// not fire for the facts written in between. Sabotage: keep orphan rows.
func TestDispatch_RemovedTriggerForgets(t *testing.T) {
	t.Parallel()
	_, ri := newTriggerRepo(t, trig("all", "learn", "", ""))
	write(t, ri, "kb/tasks/one.md")
	setOntology(t, ri, triggerOntology(""))
	require.NotContains(t, watermarks(t, ri), "all", "the name is gone: its bookmark is deleted")
	write(t, ri, "kb/tasks/between.md")
	setOntology(t, ri, triggerOntology("", trig("all", "learn", "", "")))
	write(t, ri, "kb/tasks/three.md")
	require.Equal(t, []string{"kb/tasks/three.md", "kb/tasks/one.md"}, pathsOf(firesOf(t, ri, "all")))
}

// InvalidTriggerFreezesNotForgets [M3]: break a trigger's glob (it is
// `invalid`), write a matching fact, fix the glob: the fact fires. The
// bookmark froze; it was not deleted. Sabotage: delete the watermark when the
// trigger is invalid.
func TestDispatch_InvalidTriggerFreezesNotForgets(t *testing.T) {
	t.Parallel()
	_, ri := newTriggerRepo(t, trig("all", "learn", "tasks/**", ""))
	write(t, ri, "kb/tasks/one.md")
	frozenAt := head(t, ri)
	setOntology(t, ri, triggerOntology("", trig("all", "learn", "tasks/{user}/**", "")))
	require.Equal(t, frozenAt, watermarks(t, ri)["all"], "the bookmark is frozen, not deleted")
	rep, err := ri.TriggerReport(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, "frozen", rep.Triggers[0].State)
	require.Contains(t, rep.Triggers[0].Error, "{user}")
	write(t, ri, "kb/tasks/while-broken.md")
	require.Equal(t, frozenAt, watermarks(t, ri)["all"])
	require.Len(t, firesOf(t, ri, "all"), 1)
	setOntology(t, ri, triggerOntology("", trig("all", "learn", "tasks/**", "")))
	require.Equal(t, []string{"kb/tasks/while-broken.md", "kb/tasks/one.md"}, pathsOf(firesOf(t, ri, "all")),
		"the fix catches up on what happened while the trigger was invalid")
	require.Equal(t, head(t, ri), watermarks(t, ri)["all"])
}

// UnsupportedNoBackfill (user ruling 2): a trigger with a reserved `do` is
// `unsupported`, not invalid: no ERROR, no bookmark. When it becomes active
// (here: the author changes it to emit), it starts at the head of THAT
// advance and fires only for later changes. Sabotage: set or keep a bookmark
// for an unsupported trigger.
func TestDispatch_UnsupportedNoBackfill(t *testing.T) {
	logs := captureLogs(t, zerolog.ErrorLevel)
	m := newTestManager(t)
	ri := bootRepo(t, m)
	// Since F07 PR 5 no `do` value is reserved: `run` stands in for one by
	// being deactivated for this part, as an older binary saw it.
	restore := fact.DeactivateTriggerDoForTest(fact.TriggerDoRun)
	run := "      - {name: later, on: learn, do: run, recipe: worker}\n"
	setOntology(t, ri, triggerOntology("", run))
	write(t, ri, "kb/tasks/while-unsupported.md")
	require.NotContains(t, watermarks(t, ri), "later", "an unsupported trigger holds no bookmark")
	rep, err := ri.TriggerReport(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, "unsupported", rep.Triggers[0].State)
	require.Contains(t, rep.Triggers[0].Error, "not supported")
	require.Equal(t, 0, countLines(logs, "invalid trigger skipped"), "unsupported is not an error")
	restore()

	// T13-activation (F07 PR 5): with `run` active again, the SAME entry —
	// declared while unsupported, so it holds no bookmark — bookmarks the head
	// of the first advance this version sees and never fires for earlier
	// writes. Nothing is bound (no recipe), so a later fire is an unbound
	// no-op: counted, no row.
	runOn := setOntology(t, ri, triggerOntology("", run, trig("marker", "learn", "", "")))
	require.Equal(t, runOn, watermarks(t, ri)["later"], "a run trigger bookmarks the head of the advance that activates it")
	require.Empty(t, firesOf(t, ri, "later"), "no back-fill on activation")
	write(t, ri, "kb/tasks/after-run.md")
	require.Empty(t, firesOf(t, ri, "later"), "unbound writes no row")
	require.Equal(t, int64(1), ri.triggers.stats.view("later").Unbound)

	enabled := setOntology(t, ri, triggerOntology("", trig("later", "learn", "", "")))
	require.Equal(t, enabled, watermarks(t, ri)["later"], "first set at the advance where it becomes active")
	write(t, ri, "kb/tasks/after-enable.md")
	require.Equal(t, []string{"kb/tasks/after-enable.md"}, pathsOf(firesOf(t, ri, "later")), "no back-fill")

	// The activation half for `do: script` (T20): the SAME rule carries a
	// trigger from unsupported (an older binary's view: the `push` above
	// `run` stands in for it) to active — the bookmark is set at the advance where
	// the active declaration appears, and the writes made meanwhile never
	// fire. Sabotage: bookmark the unsupported trigger.
	write(t, ri, "kb/tasks/before-script.md")
	scriptOn := setOntology(t, ri, triggerOntology("", "      - {name: scripted, on: learn, do: script, script: inbox-dispatch}\n"))
	require.Equal(t, scriptOn, watermarks(t, ri)["scripted"], "a script trigger bookmarks the head of the advance that activates it")
	rep, err = ri.TriggerReport(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, "inbox-dispatch", rep.Triggers[0].Script)
	require.Empty(t, firesOf(t, ri, "scripted"), "no back-fill on activation")

	// T10 (F07 PR 4): the same rule for `do: push` — declared under an older
	// binary it was unsupported and held no bookmark; activated here it
	// bookmarks the activating advance and never fires for earlier writes.
	write(t, ri, "kb/tasks/before-push.md")
	pushOn := setOntology(t, ri, triggerOntology("", "      - {name: fast, on: learn, do: push}\n"))
	require.Equal(t, pushOn, watermarks(t, ri)["fast"], "a push trigger bookmarks the head of the advance that activates it")
	rep, err = ri.TriggerReport(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, "active", rep.Triggers[0].State)
	require.Empty(t, firesOf(t, ri, "fast"), "no back-fill on activation")

	// Since F07 PR 2 a `due` trigger is ACTIVE (its sweep is triggers_due_test.go);
	// only a reserved `do` is unsupported now.
	enabled = setOntology(t, ri, triggerOntology("", trig("expiring", "[learn, due]", "", "")))
	rep, err = ri.TriggerReport(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, "active", rep.Triggers[0].State)
	require.Equal(t, enabled, watermarks(t, ri)["expiring"], "an active due trigger bookmarks its learn side like any other")
	write(t, ri, "kb/tasks/undated.md")
	require.Equal(t, []string{"kb/tasks/undated.md"}, pathsOf(firesOf(t, ri, "expiring")), "the learn side fires; an undated fact is never due")
}

// ErrorLoggedOncePerBlob: an invalid trigger is reported at ERROR once per
// (repo, name, ontology blob), not on every advance; siblings stay active.
// Sabotage: log on every run.
func TestDispatch_ErrorLoggedOncePerBlob(t *testing.T) {
	logs := captureLogs(t, zerolog.ErrorLevel)
	_, ri := newTriggerRepo(t,
		trig("bad", "learn", "tasks/{user}/**", ""),
		trig("good", "learn", "", ""))
	for i := 0; i < 3; i++ {
		write(t, ri, fmt.Sprintf("kb/tasks/%d.md", i))
	}
	require.Equal(t, 1, countLines(logs, "invalid trigger skipped"), "%s", logs.String())
	require.Contains(t, logs.String(), `"trigger":"bad"`)
	require.Len(t, firesOf(t, ri, "good"), 3, "siblings are unaffected")
	// A NEW blob with the same problem is reported again (once).
	setOntology(t, ri, triggerOntology("",
		trig("bad", "learn", "tasks/{user}/**", ""),
		trig("good", "learn", "", ""),
		trig("third", "learn", "", "")))
	write(t, ri, "kb/tasks/x.md")
	require.Equal(t, 2, countLines(logs, "invalid trigger skipped"))
}

// ReloadOnHeadOntology (D-b): a commit that adds a trigger makes it active
// from the next advance without a restart; a broken ontology at the head
// keeps the last good set. Sabotage: read ri.Ontology() (the open-time
// snapshot, which never changes).
func TestDispatch_ReloadOnHeadOntology(t *testing.T) {
	logs := captureLogs(t, zerolog.ErrorLevel)
	m := newTestManager(t)
	ri := bootRepo(t, m)
	require.Empty(t, ri.Ontology().Topics["tasks"], "fixture: the open-time ontology has no tasks trigger")
	setOntology(t, ri, triggerOntology("", trig("all", "learn", "", "")))
	write(t, ri, "kb/tasks/one.md")
	require.Len(t, firesOf(t, ri, "all"), 1, "active from the next advance, no restart")

	// A broken ontology at the head: ERROR once, the last good set stays.
	r, err := testService(t, ri).Facts().WriteFact(context.Background(), trigAgent, OntologyPath, "id: [broken\n", "break", "updated")
	require.NoError(t, err)
	waitTriggerHead(t, ri, r.CommitHash)
	write(t, ri, "kb/tasks/two.md")
	write(t, ri, "kb/tasks/three.md")
	require.Len(t, firesOf(t, ri, "all"), 3, "the last good trigger set keeps firing")
	require.Equal(t, 1, countLines(logs, "ontology at head unusable"), "%s", logs.String())
}

// ---- Branch filter, kick, ordering

// NoFireOnMainOrExp: main advancing (AdvanceLocalUpstream) and an exp/* write
// do not even RUN the dispatcher (the kick is filtered on the agent branch);
// CommitExperiment fires. Sabotage: drop the branch filter.
func TestDispatch_NoFireOnMainOrExp(t *testing.T) {
	t.Parallel()
	_, ri := newTriggerRepo(t, trig("all", "learn", "", ""))
	ctx := context.Background()
	svc := testService(t, ri)
	write(t, ri, "kb/tasks/own.md")
	_, seq := ri.triggers.runSequence()

	_, err := svc.AdvanceLocalUpstream(ctx, trigAgent, "main")
	require.NoError(t, err)
	_, err = svc.Experiments().OpenExperiment(ctx, "e", "", trigAgent)
	require.NoError(t, err)
	writeOn(t, ri, "exp/e", "kb/tasks/in-exp.md")
	time.Sleep(200 * time.Millisecond)
	_, seq2 := ri.triggers.runSequence()
	require.Equal(t, seq, seq2, "main and exp/* commits must not kick the dispatcher")
	require.Len(t, firesOf(t, ri, "all"), 1)

	_, err = svc.Experiments().CommitExperiment(ctx, "e", nil)
	require.NoError(t, err)
	waitTriggerHead(t, ri, head(t, ri))
	require.Equal(t, []string{"kb/tasks/in-exp.md", "kb/tasks/own.md"}, pathsOf(firesOf(t, ri, "all")))
}

// TriggerKickNeverBlocksWriter: with the worker parked, 100 writes complete;
// on release exactly ONE run follows, covering the whole burst (range_from =
// the pre-burst head, range_to = the final head). Sabotage: a blocking send
// (the writes deadlock), or an unbounded queue (100 runs).
func TestDispatch_TriggerKickNeverBlocksWriter(t *testing.T) {
	_, ri := newTriggerRepo(t, trig("all", "learn", "", ""))
	h0 := write(t, ri, "kb/tasks/warm.md")
	release := parkDispatcher(t, ri)
	done := make(chan string, 1)
	go func() {
		var last string
		for i := 0; i < 100; i++ {
			last = writeOn(t, ri, trigAgent, fmt.Sprintf("kb/tasks/burst-%03d.md", i))
		}
		done <- last
	}()
	var final string
	select {
	case final = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the writes did not complete while the dispatcher was parked: the kick blocked the writer")
	}
	runsBefore := len(runRows(t, ri))
	release()
	waitTriggerHead(t, ri, final)
	time.Sleep(200 * time.Millisecond) // a pending kick may run once more (W == H: no row)
	rows := runRows(t, ri)
	require.Len(t, rows, runsBefore+1, "exactly one run for the whole burst")
	require.Equal(t, h0, rows[0].RangeFrom)
	require.Equal(t, final, rows[0].RangeTo)
	require.Equal(t, 100, rows[0].Paths)
	require.Len(t, firesOf(t, ri, "all"), 101)
}

// KickReceivedBeforeRefRead [S1]: the kick is consumed BEFORE the head ref is
// read, so a commit that lands right after the ref read leaves its own kick
// pending and fires in the next run without any further commit. Sabotage:
// drain the kick channel after reading the ref (the in-between commit's kick
// is swallowed and its fire waits for an unrelated later commit).
func TestDispatch_KickReceivedBeforeRefRead(t *testing.T) {
	_, ri := newTriggerRepo(t, trig("all", "learn", "", ""))
	write(t, ri, "kb/tasks/warm.md")
	var once sync.Once
	var inBetween string
	setHooks(t, triggerHooks{afterHeadRead: func() {
		once.Do(func() { inBetween = writeOn(t, ri, trigAgent, "kb/tasks/in-between.md") })
	}})
	writeOn(t, ri, trigAgent, "kb/other/unrelated.md") // the kick whose run the hook interrupts
	require.Eventually(t, func() bool {
		return len(firesOf(t, ri, "all")) == 2
	}, 10*time.Second, 10*time.Millisecond, "the in-between commit (%s) must fire with no further commit", inBetween)
	require.Equal(t, inBetween, firesOf(t, ri, "all")[0].Commit)
}

// OneRunRowPerRun [M2]: a burst of 5 non-matching writes with 50 triggers
// declared gives exactly ONE run row, with range_from/range_to and evaluated,
// and zero per-trigger rows. Sabotage: one row per trigger per run.
func TestDispatch_OneRunRowPerRun(t *testing.T) {
	entries := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		entries = append(entries, trig(fmt.Sprintf("t%02d", i), "learn", "tasks/x/**", ""))
	}
	_, ri := newTriggerRepo(t, entries...)
	h0 := head(t, ri)
	release := parkDispatcher(t, ri)
	var last string
	for i := 0; i < 5; i++ {
		last = writeOn(t, ri, trigAgent, fmt.Sprintf("kb/tasks/y/%d.md", i))
	}
	release()
	waitTriggerHead(t, ri, last)
	time.Sleep(150 * time.Millisecond)
	all := fires(t, ri)
	require.Len(t, all, 1, "exactly one row: the run row; got %+v", all)
	require.Equal(t, store.TriggerOutcomeRun, all[0].Outcome)
	require.Equal(t, h0, all[0].RangeFrom)
	require.Equal(t, last, all[0].RangeTo)
	require.Equal(t, 5, all[0].Paths)
	require.Equal(t, 0, all[0].Evaluated)
}

// CrashBetweenFireAndWatermark: a panic between tx1 and tx2 leaves the fire
// rows logged and the watermark behind; on restart the range re-fires, the
// fire log shows both runs' rows, and the watermark ends at H. Sabotage:
// move the watermark update into tx1, or before the emits.
func TestDispatch_CrashBetweenFireAndWatermark(t *testing.T) {
	home := t.TempDir()
	deps := Deps{Cfg: config.Config{Home: home, OntologyRoot: "kb"}, AgentBranch: trigAgent,
		KeyPath: filepath.Join(home, "agent.key"), Machine: Options{Synchronous: true, CrashBackoff: testCrashBackoff}}
	m := New(context.Background(), deps)
	ri := bootRepo(t, m)
	setOntology(t, ri, triggerOntology("", trig("all", "learn", "", "")))
	w := head(t, ri)
	var once sync.Once
	setHooks(t, triggerHooks{beforeTx2: func() {
		once.Do(func() { panic("crash between tx1 and tx2") })
	}})
	h := write(t, ri, "kb/tasks/crash.md")
	require.Len(t, firesOf(t, ri, "all"), 1, "tx1 committed before the crash")
	require.Equal(t, w, watermarks(t, ri)["all"], "tx2 never ran: the bookmark is behind")
	require.NoError(t, m.Close())

	m2 := New(context.Background(), deps)
	require.NoError(t, m2.Start())
	t.Cleanup(func() { _ = m2.Close() })
	ri2 := m2.Get(testRepoName)
	require.NotNil(t, ri2)
	waitTriggerHead(t, ri2, h)
	got := firesOf(t, ri2, "all")
	require.Len(t, got, 2, "the range re-fired on restart; both runs are in the log")
	require.Equal(t, h, watermarks(t, ri2)["all"])
	require.Len(t, runRows(t, ri2), 2)
}

// NonlinearRewindDiffs: after a rewind the old head is not an ancestor of the
// new one. The dispatcher still diffs the trees: the scrubbed path fires as a
// retract with commit = the new head, author.kind = unknown, source = merged;
// the new path fires as a learn; both rows are marked nonlinear. Sabotage:
// use ChangesUnder (refuses, nothing fires), or walk for the retract (returns
// the root commit).
func TestDispatch_NonlinearRewindDiffs(t *testing.T) {
	t.Parallel()
	_, ri := newTriggerRepo(t,
		trig("all", "[learn, retract]", "", ""),
		trig("unknown-author", "[learn, retract]", "", "change.author.kind === 'unknown'"))
	svc := testService(t, ri)
	base := head(t, ri)
	hA := write(t, ri, "kb/tasks/scrubbed.md")
	require.Equal(t, hA, watermarks(t, ri)["all"])

	// The rewind: the branch goes back to base (hA is no longer in its
	// history) and a new commit lands on top.
	require.NoError(t, svc.TestingSetRef("refs/heads/"+trigAgent, base))
	hC := write(t, ri, "kb/tasks/new.md")
	linear, err := svc.Triggers().IsAncestor(context.Background(), plumbing.NewHash(hA), plumbing.NewHash(hC))
	require.NoError(t, err)
	require.False(t, linear, "fixture: the advance must be nonlinear")

	got := map[string]store.TriggerFire{}
	for _, f := range firesOf(t, ri, "all") {
		if f.RangeFrom == hA {
			got[f.Path] = f
		}
	}
	require.Len(t, got, 2, "%+v", got)
	scrubbed := got["kb/tasks/scrubbed.md"]
	require.Equal(t, "retract", scrubbed.Episode)
	require.Equal(t, hC, scrubbed.Commit, "a scrubbed path has no originating commit: the fallback is the new head")
	require.Equal(t, "merged", scrubbed.Source)
	require.True(t, scrubbed.Nonlinear)
	learned := got["kb/tasks/new.md"]
	require.Equal(t, "learn", learned.Episode)
	require.Equal(t, hC, learned.Commit)
	require.True(t, learned.Nonlinear)
	require.Equal(t, []string{"kb/tasks/scrubbed.md"}, pathsOf(firesOf(t, ri, "unknown-author")),
		"only the scrubbed path has an unknown author")
	require.Equal(t, hC, watermarks(t, ri)["all"])
}

// ---- Lifecycle

// SurvivesSyncRestart: the dispatcher runs under the Serve stage's life, not
// Sync's. An origin detach restarts Sync (and only Sync); a write afterwards
// still fires. Sabotage: run the dispatcher under the Sync life.
func TestDispatch_SurvivesSyncRestart(t *testing.T) {
	t.Parallel()
	m, ri := newTriggerRepo(t, trig("all", "learn", "", ""))
	write(t, ri, "kb/tasks/before.md")
	serveGen, syncGen := ri.Status().gens[StageServe], ri.Status().gens[StageSync]
	_, err := m.Send(context.Background(), ri, DetachOrigin())
	require.NoError(t, err)
	require.Equal(t, serveGen, ri.Status().gens[StageServe], "Serve was not touched")
	require.NotEqual(t, syncGen, ri.Status().gens[StageSync], "Sync was restarted")
	write(t, ri, "kb/tasks/after.md")
	require.Equal(t, []string{"kb/tasks/after.md", "kb/tasks/before.md"}, pathsOf(firesOf(t, ri, "all")))
}

// SurvivesSwapStore: a write after SwapStore fires. The kick lives in
// ri.onCommit, which SwapStore re-applies; the dispatcher acquires the store
// per phase, so it follows the swap. Sabotage: register the kick on
// *store.Service (stranded by the swap).
func TestDispatch_SurvivesSwapStore(t *testing.T) {
	t.Parallel()
	m, ri := newTriggerRepo(t, trig("all", "learn", "", ""))
	write(t, ri, "kb/tasks/before.md")
	// Swap in a copy of this repo's own database (the same history).
	require.NoError(t, ri.WithRead(func(svc *store.Service) { require.NoError(t, svc.Checkpoint()) }))
	tmp := filepath.Join(t.TempDir(), "copy.db")
	copyDB(t, m.RepoPath(ri.UID()), tmp)
	require.NoError(t, swapStore(m, ri, tmp))
	write(t, ri, "kb/tasks/after.md")
	require.Equal(t, []string{"kb/tasks/after.md", "kb/tasks/before.md"}, pathsOf(firesOf(t, ri, "all")))
}

// NoStoreHeldDuringIf [M1]: while a run is inside slow `if`s, SwapStore
// completes without waiting for it, and teardown stops the run within one
// evaluation. Sabotage: hold Acquire across phase B.
func TestDispatch_NoStoreHeldDuringIf(t *testing.T) {
	m, ri := newTriggerRepo(t, trig("slow", "learn", "", ""))
	write(t, ri, "kb/tasks/warm.md")
	slowIf(t, "slow", 90, nil)
	release := parkDispatcher(t, ri)
	for i := 0; i < 30; i++ {
		writeOn(t, ri, trigAgent, fmt.Sprintf("kb/tasks/p%02d.md", i))
	}
	release()
	time.Sleep(400 * time.Millisecond) // phase A is done; the run is in phase B (30 × 90 ms)

	require.NoError(t, ri.WithRead(func(svc *store.Service) { require.NoError(t, svc.Checkpoint()) }))
	tmp := filepath.Join(t.TempDir(), "copy.db")
	copyDB(t, m.RepoPath(ri.UID()), tmp)
	started := time.Now()
	require.NoError(t, swapStore(m, ri, tmp))
	require.Less(t, time.Since(started), time.Second, "a swap must not drain behind a run's if phase")

	started = time.Now()
	restartServe(t, ri)
	require.Less(t, time.Since(started), 500*time.Millisecond, "cancel stops the run within one evaluation")
}

// NotStartedReadOnlyOrSubscribed: no dispatcher on a read-only server and none
// for a subscription; the report says why. Sabotage: drop the guard.
func TestDispatch_NotStartedReadOnlyOrSubscribed(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", ReadOnly: true},
		AgentBranch: trigAgent,
		KeyPath:     filepath.Join(home, "agent.key"),
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	t.Cleanup(func() { _ = m.Close() })
	ri := bootRepo(t, m)
	require.Nil(t, ri.triggers, "a read-only server runs no dispatcher")
	rep, err := ri.TriggerReport(context.Background(), 0)
	require.NoError(t, err)
	require.False(t, rep.Enabled)
	require.Equal(t, "read-only server", rep.Reason)

	_, sub, _ := buildSubscription(t)
	require.Nil(t, sub.triggers, "a subscription has no agent branch to observe")
	rep, err = sub.TriggerReport(context.Background(), 0)
	require.NoError(t, err)
	require.False(t, rep.Enabled)
	require.Contains(t, rep.Reason, "subscription")
}

// ---- if

// IfThrowsIsFalse / IfTimeoutIsFalse: a throwing or timing-out `if` counts as
// false, is recorded (if-error / if-timeout), and the run continues: the
// watermark advances and a sibling still fires. Sabotage: propagate the error.
func TestDispatch_IfThrowsOrTimesOutIsFalse(t *testing.T) {
	t.Parallel()
	_, ri := newTriggerRepo(t,
		trig("throws", "learn", "", "fact.nope.deeper === 1"),
		trig("loops", "learn", "", "(function(){ while (true) {} })()"),
		trig("all", "learn", "", ""))
	h := write(t, ri, "kb/tasks/x.md")
	require.Equal(t, h, watermarks(t, ri)["throws"])
	require.Equal(t, h, watermarks(t, ri)["loops"])
	th := firesOf(t, ri, "throws")
	require.Len(t, th, 1)
	require.Equal(t, store.TriggerOutcomeIfError, th[0].Outcome)
	require.Contains(t, th[0].Error, "deeper", "the JS error is recorded verbatim")
	lo := firesOf(t, ri, "loops")
	require.Len(t, lo, 1)
	require.Equal(t, store.TriggerOutcomeIfTimeout, lo[0].Outcome)
	require.Len(t, firesOf(t, ri, "all"), 1)
	st := ri.triggers.stats.view("throws")
	require.Equal(t, int64(1), st.IfError)
	require.Equal(t, int64(0), st.Fires)
	require.Equal(t, int64(1), ri.triggers.stats.view("loops").IfTimeout)
}

// An unparseable fact fires with fact = null (the outcome says so) and a
// condition that reads it throws → false.
func TestDispatch_UnparseableFact(t *testing.T) {
	t.Parallel()
	_, ri := newTriggerRepo(t,
		trig("all", "learn", "", ""),
		trig("reads-fact", "learn", "", "fact.type === 'observation'"))
	r, err := testService(t, ri).Facts().WriteFact(context.Background(), trigAgent, "kb/tasks/bad.md", "no frontmatter at all\n", "bad", "learn")
	require.NoError(t, err)
	waitTriggerHead(t, ri, r.CommitHash)
	all := firesOf(t, ri, "all")
	require.Len(t, all, 1)
	require.Equal(t, store.TriggerOutcomeUnparseable, all[0].Outcome)
	rf := firesOf(t, ri, "reads-fact")
	require.Len(t, rf, 1)
	require.Equal(t, store.TriggerOutcomeIfError, rf[0].Outcome)
}

// ---- emit

// EmitReachesStreamWithoutDebounce: under a background writer committing
// every 150 ms on another topic, the trigger event for one matching write
// reaches a hub subscriber within 500 ms. Sabotage: route the emit through
// the 1 s debounced observer (it arrives only after the writes stop).
func TestDispatch_EmitReachesStreamWithoutDebounce(t *testing.T) {
	_, ri := newTriggerRepo(t, trig("all", "learn", "", ""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, _ := ri.TaskHub().Subscribe(ctx)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(150 * time.Millisecond):
				writeOn(t, ri, trigAgent, fmt.Sprintf("kb/other/bg-%d.md", i))
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	started := time.Now()
	c := writeOn(t, ri, trigAgent, "kb/tasks/match.md")
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case e := <-events:
			if ev, ok := e.(TriggerEvent); ok && ev.Path == "kb/tasks/match.md" {
				require.Equal(t, trigAgent, ev.Branch)
				require.Equal(t, "all", ev.Trigger)
				require.Equal(t, "learn", ev.Episode)
				require.Equal(t, "local", ev.Source)
				require.Equal(t, c, ev.Commit)
				t.Logf("trigger event after %s", time.Since(started))
				close(stop)
				wg.Wait()
				return
			}
		case <-deadline:
			close(stop)
			wg.Wait()
			t.Fatal("the trigger event did not arrive within 500 ms under steady writes")
		}
	}
}

// copyDB copies a repo database for a swap.
func copyDB(t *testing.T, src, dst string) {
	t.Helper()
	require.NoError(t, copyFile(src, dst))
}
