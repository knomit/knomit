package repos

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// The three answers, against real bare repos on disk.
//
// "Is there a knomit ontology on this branch?" is the ONE question that decides
// whether the wizard joins, initializes, or blocks — so each answer is pinned
// separately, and the third one hardest: an unknown must never render as
// either of the other two.

func TestProbeInitialized_YesWhenTheBranchCarriesAnOntology(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemote(t, filepath.Join(dir, "remote.git")) // WITH an ontology
	m := newRemoteModeManager(t, dir)

	got, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url, Branch: "main"})
	require.NoError(t, err)
	require.Equal(t, InitializedYes, got.Initialized)
	require.Equal(t, "main", got.Branch)
	require.Empty(t, got.Detail)
}

func TestProbeInitialized_NoWhenTheBranchHasNone(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemoteNoOntology(t, filepath.Join(dir, "remote.git"))
	m := newRemoteModeManager(t, dir)

	got, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url, Branch: "main"})
	require.NoError(t, err)
	require.Equal(t, InitializedNo, got.Initialized)
	require.Equal(t, "main", got.Branch)
}

// A check that could not run establishes NOTHING, and must say so rather than
// collapsing into "no". Guessing "no" here writes an ontology over a knowledge
// base that already had one — unrecoverable, because the ontology is immutable
// after creation.
func TestProbeInitialized_UnknownWhenTheRemoteCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	m := newRemoteModeManager(t, dir)

	got, err := m.ProbeInitialized(context.Background(),
		OriginSpec{URL: "file://" + filepath.Join(dir, "does-not-exist.git"), Branch: "main"})
	require.NoError(t, err, "an unreadable remote is a RESULT, not an error")
	require.Equal(t, InitializedUnknown, got.Initialized,
		"a failed check must be the third state, never 'no'")
	require.NotEmpty(t, got.Detail, "an unknown must carry the reason it is unknown")
}

// Naming a branch the remote does not have is also an unknown: nothing was
// looked at. It is emphatically not "that branch has no ontology".
func TestProbeInitialized_UnknownWhenTheBranchDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemote(t, filepath.Join(dir, "remote.git"))
	m := newRemoteModeManager(t, dir)

	got, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url, Branch: "no-such-branch"})
	require.NoError(t, err)
	require.Equal(t, InitializedUnknown, got.Initialized)
	require.NotEmpty(t, got.Detail)
}

// A remote with no refs has no branch to answer about. Reporting it as "no"
// would invite an initialize that has nothing to cut an agent branch from.
func TestProbeInitialized_EmptyRemoteIsUnknownAndSaysWhy(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())
	m := newRemoteModeManager(t, dir)

	got, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: "file://" + remoteDir})
	require.NoError(t, err)
	require.Equal(t, InitializedUnknown, got.Initialized)
	require.Contains(t, got.Detail, "one commit is enough")
}

// The answer is PER BRANCH — that is the entire reason this is a separate
// endpoint from the origin probe, which runs before a branch is known. A repo
// can carry the ontology on main and not on develop, and a check that ignored
// the branch would route the develop case to the wrong mode.
func TestProbeInitialized_AnswersPerBranchNotPerRemote(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemote(t, filepath.Join(dir, "remote.git")) // main HAS an ontology
	bare := bareOf(url)

	// A second branch cut from main with the ontology REMOVED.
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	runGit(t, work, "checkout", "-b", "develop")
	require.NoError(t, os.Remove(filepath.Join(work, OntologyPath)))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "drop the ontology")
	runGit(t, work, "push", "origin", "develop")

	m := newRemoteModeManager(t, dir)

	onMain, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url, Branch: "main"})
	require.NoError(t, err)
	require.Equal(t, InitializedYes, onMain.Initialized)

	onDevelop, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url, Branch: "develop"})
	require.NoError(t, err)
	require.Equal(t, InitializedNo, onDevelop.Initialized,
		"the same remote must answer differently for a branch without the ontology")
}

// With no branch named, the remote's own default is inspected and REPORTED, so
// the caller learns which branch the answer is about rather than assuming.
func TestProbeInitialized_DefaultsToTheRemoteHeadAndNamesIt(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemote(t, filepath.Join(dir, "remote.git"))
	m := newRemoteModeManager(t, dir)

	got, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url})
	require.NoError(t, err)
	require.Equal(t, InitializedYes, got.Initialized)
	require.Equal(t, "main", got.Branch, "the branch actually inspected must be reported back")
}

// The local-origin gate admits no exemption, and this probe genuinely CLONES —
// so it gates like every other clone/fetch path rather than being the one hole.
func TestProbeInitialized_RejectsUngatedLocalPath(t *testing.T) {
	m := newRemoteModeManager(t, t.TempDir()) // a root that does NOT contain /etc
	_, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: "/etc", Branch: "main"})
	require.Error(t, err, "expected the local-origin gate to reject an out-of-root path")
}

// Legacy rungs count. A repo that predates the move to .knomit/ and still holds
// domains/ontology.yaml IS a knowledge base — reporting it as uninitialized
// would offer to write a second ontology over the one that already governs it.
func TestProbeInitialized_YesForALegacyOntologyPath(t *testing.T) {
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)

	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	// The OLDEST rung — the one a repo that skipped every hand-migration holds.
	require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(PreDotOntologyPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, PreDotOntologyPath), ont, 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "legacy ontology")
	runGit(t, work, "push", "origin", "main")

	m := newRemoteModeManager(t, dir)
	got, perr := m.ProbeInitialized(context.Background(), OriginSpec{URL: "file://" + bare, Branch: "main"})
	require.NoError(t, perr)
	require.Equal(t, InitializedYes, got.Initialized,
		"a legacy ontology rung still makes the repo a knowledge base")
}

// seedRemoteInitializedByThisMachine reproduces the state a SUCCESSFUL
// initialize leaves behind: the consensus branch untouched and without an
// ontology, and this machine's agent branch carrying one, never merged.
//
// That is not an exotic state — it is the steady state of the mode this branch
// was built around. knomit never writes to the consensus branch, so every
// initialized remote looks like this until someone merges the agent branch.
func seedRemoteInitializedByThisMachine(t *testing.T, bare, agentBranch string) string {
	t.Helper()
	url := seedBareRemoteNoOntology(t, bare)

	work := t.TempDir()
	runGit(t, "", "clone", bareOf(url), work)
	runGit(t, work, "checkout", "-b", agentBranch)
	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(OntologyPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, OntologyPath), ont, 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "init: create knowledge base")
	runGit(t, work, "push", "origin", agentBranch)
	return url
}

// THE RE-CREATE DEAD END.
//
// The probe inspected the CONSENSUS branch while the create checks the branch
// InitFromRemote actually ADOPTS — and those are different branches whenever
// this machine's agent branch already exists on the remote (store/repo.go:
// "If origin/agent/<host> exists, adopt it"). So re-creating a repo this
// machine had already initialized was a permanent dead end: the consensus
// branch has no ontology and never will, so the probe said "no", the wizard
// derived mode "initialize" — the only mode it CAN derive from that answer —
// and initInitialize refused it with ErrRemoteAlreadyInitialized. "Try again"
// reproduced it forever, and agentBranchAlreadyPushed told the user the
// opposite: that creating again would adopt that branch.
//
// The probe must answer the question the create will actually ask.
func TestProbeInitialized_YesWhenThisMachineAlreadyInitializedTheRemote(t *testing.T) {
	dir := t.TempDir()
	url := seedRemoteInitializedByThisMachine(t, filepath.Join(dir, "remote.git"), "agent/test-abc")
	m := newRemoteModeManager(t, dir)

	got, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url, Branch: "main"})
	require.NoError(t, err)
	require.Equal(t, InitializedYes, got.Initialized,
		"a create here ADOPTS the agent branch, which is already a knowledge base — answering 'no' routes the wizard to the one mode that cannot succeed")
	require.Equal(t, "agent/test-abc", got.Branch,
		"the branch actually inspected must be reported, or the UI names the wrong one")
}

// The mirror, and the reason this cannot simply always prefer the agent branch:
// a remote NOBODY has initialized still answers about the consensus branch.
func TestProbeInitialized_NoWhenNoAgentBranchExistsOnTheRemote(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemoteNoOntology(t, filepath.Join(dir, "remote.git"))
	m := newRemoteModeManager(t, dir)

	got, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url, Branch: "main"})
	require.NoError(t, err)
	require.Equal(t, InitializedNo, got.Initialized)
	require.Equal(t, "main", got.Branch)
}

// A DIFFERENT machine's agent branch must not be adopted or inspected:
// InitFromRemote adopts origin/agent/<host> for THIS host only, so a remote
// initialized by someone else is still "no" for us — we cut our own agent
// branch from the consensus branch, and the ontology question is about that.
func TestProbeInitialized_IgnoresAnotherMachinesAgentBranch(t *testing.T) {
	dir := t.TempDir()
	url := seedRemoteInitializedByThisMachine(t, filepath.Join(dir, "remote.git"), "agent/some-other-box")
	m := newRemoteModeManager(t, dir)

	got, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url, Branch: "main"})
	require.NoError(t, err)
	require.Equal(t, InitializedNo, got.Initialized,
		"another machine's agent branch is not adopted, so it must not answer this question")
	require.Equal(t, "main", got.Branch)
}

// THE AGREEMENT TEST.
//
// store.BranchACreateReads is one function, but it is applied to two different
// inputs: the refs a remote advertises (here, before any fetch) and the
// remote-tracking refs InitFromRemote has just pulled. Sharing the function
// removes one way for those to disagree; it does not remove the possibility
// that the two callers feed it different facts.
//
// So this pins the property directly, for each shape of remote: the branch the
// probe SAYS a create will read, and then — the part the dead end failed — that
// the mode derived from that answer is one that actually succeeds. A probe
// whose answer routes to a mode the create refuses is the bug, whatever the
// two implementations happen to look like.
func TestProbeInitialized_PredictsWhatTheCreateWillDo(t *testing.T) {
	cases := []struct {
		name       string
		seed       func(t *testing.T, bare, agentBranch string) string
		wantBranch func(agentBranch string) string
		wantAnswer string
	}{
		{
			name: "remote carries this machine's agent branch",
			seed: func(t *testing.T, bare, agent string) string {
				return seedRemoteInitializedByThisMachine(t, bare, agent)
			},
			wantBranch: func(agent string) string { return agent },
			wantAnswer: InitializedYes,
		},
		{
			name: "remote has never been initialized",
			seed: func(t *testing.T, bare, _ string) string {
				return seedBareRemoteNoOntology(t, bare)
			},
			wantBranch: func(string) string { return "main" },
			wantAnswer: InitializedNo,
		},
		{
			name: "remote was initialized by a DIFFERENT machine",
			seed: func(t *testing.T, bare, _ string) string {
				return seedRemoteInitializedByThisMachine(t, bare, "agent/not-this-box")
			},
			wantBranch: func(string) string { return "main" },
			wantAnswer: InitializedNo,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			m := newRemoteModeManager(t, dir)
			url := c.seed(t, filepath.Join(dir, "remote.git"), m.deps.AgentBranch)

			predicted, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url, Branch: "main"})
			require.NoError(t, err)
			require.Equal(t, c.wantBranch(m.deps.AgentBranch), predicted.Branch,
				"the probe named a different branch than the rule says a create reads")
			require.Equal(t, c.wantAnswer, predicted.Initialized)

			// The mode the wizard derives from that answer, run for real. This
			// is the assertion the dead end violated: it is not enough for the
			// probe to be self-consistent, the mode it implies has to work.
			// Built the way createBodyFor builds it (web/src/wizardState.ts):
			// clone carries NO ontology — the remote's governs, and supplying
			// one is refused rather than silently dropped — while initialize
			// carries the chosen one. Sending an ontology with clone here would
			// fail for a reason that has nothing to do with what is under test.
			spec := CreateSpec{Name: "agreed", Mode: "clone", Origin: &OriginSpec{URL: url, Branch: "main"}}
			if predicted.Initialized == InitializedNo {
				spec.Mode, spec.OntologyPreset = "initialize", "default"
			}
			mode := spec.Mode
			ri, cerr := m.Create(context.Background(), spec, func(Event) {})
			require.NoError(t, cerr, "the mode derived from the probe's answer must be one that works")
			require.NotNil(t, ri)
			require.NotNil(t, ri.Ontology(), "every repo has an ontology")

			// And it read the branch that was predicted: whatever this machine's
			// agent branch now points at on the remote must descend from it.
			// The foreign-agent case is the discriminating one — a create that
			// wrongly adopted agent/not-this-box would fail this.
			if mode == "initialize" {
				bare := bareOf(url)
				predictedTip := refHash(t, bare, c.wantBranch(m.deps.AgentBranch))
				agentTip := refHash(t, bare, m.deps.AgentBranch)
				require.NoError(t,
					exec.Command("git", "-C", bare, "merge-base", "--is-ancestor", predictedTip, agentTip).Run(),
					"the pushed agent branch does not descend from the branch the probe predicted")
			}
		})
	}
}

// THE PROBE IS BOUNDED.
//
// ProbeInitialized clones a tip into memory from a caller-supplied URL. Depth:1
// and SingleBranch bound the HISTORY, not the CONTENT: the whole tip tree and
// every blob in it are decoded onto the heap, and go-git's memory storage holds
// decoded objects at several times the packfile size. Pointed at something
// large — or called repeatedly — that is unbounded allocation on a public
// endpoint, to answer a question that needs one tree entry.
//
// Exceeding the bound is the THIRD STATE, not a "no": nothing about the branch
// was established, and the caller is told why rather than being handed an
// answer that would route a create.
func TestProbeInitialized_RefusesToPullMoreThanItsBudget(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemote(t, filepath.Join(dir, "remote.git"))
	m := newRemoteModeManager(t, dir)
	m.deps.Cfg.Git.MaxProbeBytes = 1 // one byte: any real transfer exceeds it

	got, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url, Branch: "main"})
	require.NoError(t, err, "a remote too large to inspect is a RESULT, not a request error")
	require.Equal(t, InitializedUnknown, got.Initialized,
		"a probe that could not finish must not answer the question")
	require.Contains(t, got.Detail, "too large")
}

// And the ordinary case still answers, with the budget at its default.
func TestProbeInitialized_DefaultBudgetAnswersNormalRemotes(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemote(t, filepath.Join(dir, "remote.git"))
	m := newRemoteModeManager(t, dir)

	got, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: url, Branch: "main"})
	require.NoError(t, err)
	require.Equal(t, InitializedYes, got.Initialized)
}
