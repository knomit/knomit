package repos

// Conflict merge PR 2, the two-instance matrix probe. Real transport, real
// syncs, real pushes — no merge function called by hand.
//
// T1: B hosts the repo (no origin; `consensus: auto`, so B's consensus merger
// merges every branch A pushes into B's agent branch, and B's consensus
// branch follows); A clones B. Both change ONE fact and ONE non-fact path.
// For every `conflicts` value (facts × state, plus absent → the auto default)
// the final version on B's agent branch, on B's consensus branch and on A
// after its sync is asserted, and three idle rounds must add no commit.
//
// T2: a GitHub-like origin — a bare repository whose consensus branch is
// "trunk" (not main), advanced only by a "PR merge" run with git itself. Both
// instances clone it, sync and push; both converge on trunk's version.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/platform/fileuri"
	"knomit/internal/testsupport/testsigner"
)

// statePath is a path both instances change that is not a fact: the `state`
// key settles it.
const statePath = "notes.txt"

// The two instances' changes to the shared fact, from the base (body "base",
// confidence 0.7, no entities). A is MORE confident; B is the consensus side
// wherever B decides (B hosts in T1).
func matrixVersions(t *testing.T) (base, a, b string) {
	return cmBody(t, "base", 0.7), cmBody(t, "A body", 0.95, "ea"), cmBody(t, "B body", 0.7, "eb")
}

// matrixFact is the expected merged fact, as fields.
type matrixFact struct {
	body     string
	conf     float64
	entities []string
}

var (
	factA       = matrixFact{"A body", 0.95, []string{"ea"}}
	factB       = matrixFact{"B body", 0.7, []string{"eb"}}
	factMerged  = matrixFact{"A body", 0.95, []string{"ea", "eb"}} // confidence rule: A wins body; lists union, winner first
	factMergedC = matrixFact{"B body", 0.95, []string{"eb", "ea"}} // consensus rule: B's body; A's confidence (only A changed it)
)

func requireFact(t *testing.T, where, content string, want matrixFact) {
	t.Helper()
	f := parseShared(t, content)
	require.Equal(t, want.body, f.Body, "%s: body", where)
	require.Equal(t, want.conf, f.Confidence, "%s: confidence", where)
	require.Equal(t, want.entities, f.Entities, "%s: entities", where)
}

func readPath(t *testing.T, ri *RepoInstance, branch, path string) string {
	t.Helper()
	f, err := testService(t, ri).Facts().ReadFact(context.Background(), branch, path, nil)
	require.NoError(t, err, "%s at %s", path, branch)
	return f.Content
}

func conflictsAttrs(facts, state string) string {
	s := "attributes:\n  consensus: auto\n"
	if facts == "" && state == "" {
		return s
	}
	s += "  conflicts:\n"
	if facts != "" {
		s += "    facts: " + facts + "\n"
	}
	if state != "" {
		s += "    state: " + state + "\n"
	}
	return s
}

// TestConflictsMatrix_T1_HostedAuto is T1. The expectation per case follows
// from who settles what, in this order: A pushes; B's merger settles the
// conflict as the setting says (B's own branch is the consensus side) or,
// where a key is off, refuses the WHOLE merge; A's next sync then settles
// what B refused, with the consensus branch (B's version) as the consensus
// side and its own version for a key set to off; B's next merge is clean.
//
// Both keys off is today's F1 shape, and it does NOT converge by itself: A's
// LocalWins sync keeps both of its versions, so its merged tree equals its own
// and no commit is written; B never retries the tip it refused. The conflict
// stays in B's Pushed branches list for a human — B keeps B's versions, A
// keeps A's — and nothing moves in the idle rounds. Any other value settles
// something at A's sync, which moves A's branch, and B's retry is clean.
//
// SABOTAGE (each red here as well as in store): the consensus side inverted
// at the host (hostConflictStrategy → fact.MergeSrc) → merge:consensus and
// consensus rows take A's; the auto default dropped → the "absent" row
// refuses and ends with A's versions.
func TestConflictsMatrix_T1_HostedAuto(t *testing.T) {
	type side struct {
		fact  matrixFact
		state string // who owns statePath's content: "A" or "B"
	}
	cases := []struct {
		facts, state string
		b, a         side // the final versions on B (agent and consensus branch) and on A
		refused      bool // B's first merge is refused (a key set to off left something)
	}{
		{"off", "off", side{factB, "B"}, side{factA, "A"}, true}, // left for a human
		{"off", "consensus", side{factA, "B"}, side{factA, "B"}, true},
		{"merge", "off", side{factMerged, "A"}, side{factMerged, "A"}, true},
		{"merge", "consensus", side{factMerged, "B"}, side{factMerged, "B"}, false},
		{"merge:consensus", "off", side{factMergedC, "A"}, side{factMergedC, "A"}, true},
		{"merge:consensus", "consensus", side{factMergedC, "B"}, side{factMergedC, "B"}, false},
		{"consensus", "off", side{factB, "A"}, side{factB, "A"}, true},
		{"consensus", "consensus", side{factB, "B"}, side{factB, "B"}, false},
		{"", "", side{factMerged, "B"}, side{factMerged, "B"}, false}, // absent: the auto default, merge/consensus
	}
	for _, c := range cases {
		name := fmt.Sprintf("facts=%s,state=%s", c.facts, c.state)
		if c.facts == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			h := newConsensusHost(t, triggerOntology(conflictsAttrs(c.facts, c.state)), hostOpts{})
			base, aF, bF := matrixVersions(t)
			cmWrite(t, h.ri, cHostAgent, base)
			writePath(t, h.ri, cHostAgent, statePath, "base\n")
			h.advance(t)
			p := newConsensusPeer(t, h.url)

			cmWrite(t, p.ri, cPeerAgent, aF)
			writePath(t, p.ri, cPeerAgent, statePath, "A\n")
			cmWrite(t, h.ri, cHostAgent, bF)
			writePath(t, h.ri, cHostAgent, statePath, "B\n")

			// Round 1: A pushes; B's merger settles or refuses.
			p.round(t)
			h.settle(t)
			s := h.stats()
			if c.refused {
				require.Equal(t, 1, s.Refused, "a key set to off leaves a path: the whole merge is refused")
				require.Zero(t, s.Merges)
			} else {
				require.Zero(t, s.Refused, "settled on the host: %v", s.Warnings)
				require.Equal(t, 1, s.Merges)
			}
			// Rounds 2 and 3: B's consensus branch follows; A syncs and pushes.
			for i := 0; i < 2; i++ {
				h.advance(t)
				p.round(t)
				h.settle(t)
			}
			h.advance(t)

			up := h.upstream(t)
			for _, at := range []struct {
				where  string
				ri     *RepoInstance
				branch string
				want   side
			}{{"B agent", h.ri, cHostAgent, c.b}, {"B consensus", h.ri, up, c.b}, {"A", p.ri, cPeerAgent, c.a}} {
				requireFact(t, at.where, readPath(t, at.ri, at.branch, sharedPath), at.want.fact)
				require.Equal(t, at.want.state+"\n", readPath(t, at.ri, at.branch, statePath), "%s: %s", at.where, statePath)
			}
			if c.refused {
				require.Equal(t, 1, h.stats().Refused, "one refusal, never repeated")
			}

			// Convergence: three idle rounds add no commit anywhere.
			before := h.commits(t)
			peerBefore := p.tip(t)
			for i := 0; i < 3; i++ {
				p.round(t)
				h.settle(t)
				h.advance(t)
			}
			require.Equal(t, len(before), len(h.commits(t)), "three idle rounds add no commit")
			require.Equal(t, peerBefore, p.tip(t), "A's branch does not move either")
		})
	}
}

func writePath(t *testing.T, ri *RepoInstance, branch, path, content string) {
	t.Helper()
	_, err := testService(t, ri).Facts().WriteFact(context.Background(), branch, path, content, "edit "+path, "update")
	require.NoError(t, err)
}

// ghOrigin is a bare repository standing in for GitHub: its consensus branch
// is advanced only by prMerge, a merge commit made with git itself.
type ghOrigin struct {
	bare, work, url, branch string
}

func newGHOrigin(t *testing.T, root, branch, ontology string, files map[string]string) *ghOrigin {
	t.Helper()
	bare := filepath.Join(root, "origin.git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch="+branch, bare)
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	runGit(t, work, "checkout", "-B", branch)
	files[OntologyPath] = ontology
	for p, content := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(work, p), []byte(content), 0o644))
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "seed")
	runGit(t, work, "push", "origin", branch)
	runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	return &ghOrigin{bare: bare, work: work, url: fileuri.New(bare), branch: branch}
}

// prMerge merges the pushed branch into the consensus branch the way a PR
// merge does (a merge commit), unless the branch brings no change to the
// tree — there is no PR to open then. A conflict here fails the test: each
// instance synced before it pushed.
func (o *ghOrigin) prMerge(t *testing.T, branch string) {
	t.Helper()
	runGit(t, o.work, "fetch", "origin")
	if gitSucceeds(o.work, "diff", "--quiet", "origin/"+o.branch, "origin/"+branch) {
		return
	}
	runGit(t, o.work, "checkout", "-B", o.branch, "origin/"+o.branch)
	runGit(t, o.work, "merge", "--no-ff", "-m", "Merge pull request from "+branch, "origin/"+branch)
	runGit(t, o.work, "push", "origin", o.branch)
}

// commits counts every commit reachable from the origin's refs.
func (o *ghOrigin) commits(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-list", "--all", "--count")
	cmd.Dir = o.bare
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func gitSucceeds(dir string, args ...string) bool {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run() == nil
}

type ghInstance struct {
	ri     *RepoInstance
	branch string
}

func newGHInstance(t *testing.T, root string, o *ghOrigin, agent, signer string) *ghInstance {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: root}
	cfg.Git.LocalReconcileInterval = time.Minute
	m := New(context.Background(), Deps{
		Cfg:         cfg,
		AgentBranch: agent,
		KeyPath:     filepath.Join(home, "agent.key"),
		Signer:      testsigner.Named(signer),
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	t.Cleanup(func() { m.Close() })
	require.NoError(t, m.Start())
	ri, err := m.Create(context.Background(), CreateSpec{Name: "kb", Mode: "clone", Origin: &OriginSpec{URL: o.url, Branch: o.branch}}, nil)
	require.NoError(t, err)
	require.Equal(t, o.branch, testService(t, ri).UpstreamBranch(), "the consensus branch is the origin's, not main")
	return &ghInstance{ri: ri, branch: agent}
}

func (g *ghInstance) round(t *testing.T) {
	t.Helper()
	svc := testService(t, g.ri)
	_, err := svc.Remote().Sync(context.Background(), g.branch, nil)
	require.NoError(t, err)
	_, err = svc.Remote().Push(context.Background(), g.branch, nil)
	require.NoError(t, err)
}

// TestConflictsMatrix_T2_GitHubLikeOrigin is T2, with `facts: merge` and
// `state: consensus` on trunk. A's PR lands first; B's sync then meets the
// conflict against trunk (the consensus side): the fact is merged — A's
// body by confidence, both entities — and the non-fact takes trunk's version.
// B's PR lands cleanly; A's next sync takes trunk. Both instances and trunk
// end on one version, and three idle rounds add no commit to the origin.
//
// SABOTAGE: the peer sync's consensus side inverted (mergeOpts.factMerge
// defaulting to fact.MergeDst) → B keeps its own notes.txt → trunk and A end
// on B's → red.
func TestConflictsMatrix_T2_GitHubLikeOrigin(t *testing.T) {
	root := t.TempDir()
	base, aF, bF := matrixVersions(t)
	o := newGHOrigin(t, root, "trunk",
		triggerOntology("attributes:\n  conflicts:\n    facts: merge\n    state: consensus\n"),
		map[string]string{sharedPath: base, statePath: "base\n"})
	a := newGHInstance(t, root, o, "agent/aaaa1111", "instance-a")
	b := newGHInstance(t, root, o, "agent/bbbb2222", "instance-b")

	cmWrite(t, a.ri, a.branch, aF)
	writePath(t, a.ri, a.branch, statePath, "A\n")
	cmWrite(t, b.ri, b.branch, bF)
	writePath(t, b.ri, b.branch, statePath, "B\n")

	a.round(t)
	o.prMerge(t, a.branch) // A's PR lands on trunk
	b.round(t)             // B meets the conflict against trunk
	head := headCommit(t, b.ri, b.branch)
	require.Contains(t, head.Message, "(conflicts:merge/consensus)", "B's sync settled the conflict with the setting")
	o.prMerge(t, b.branch)
	for i := 0; i < 2; i++ {
		a.round(t)
		b.round(t)
		o.prMerge(t, a.branch)
		o.prMerge(t, b.branch)
	}

	for _, at := range []struct {
		where string
		g     *ghInstance
	}{{"A", a}, {"B", b}} {
		requireFact(t, at.where, readPath(t, at.g.ri, at.g.branch, sharedPath), factMerged)
		require.Equal(t, "A\n", readPath(t, at.g.ri, at.g.branch, statePath), "%s: trunk's version of the non-fact", at.where)
		requireFact(t, at.where+" trunk", readPath(t, at.g.ri, o.branch, sharedPath), factMerged)
	}
	require.Equal(t, readPath(t, a.ri, a.branch, sharedPath), readPath(t, b.ri, b.branch, sharedPath), "one version, byte for byte")

	before := o.commits(t)
	aTip, bTip := headCommit(t, a.ri, a.branch).Hash, headCommit(t, b.ri, b.branch).Hash
	for i := 0; i < 3; i++ {
		a.round(t)
		b.round(t)
		o.prMerge(t, a.branch)
		o.prMerge(t, b.branch)
	}
	require.Equal(t, before, o.commits(t), "three idle rounds add no commit to the origin")
	require.Equal(t, aTip, headCommit(t, a.ri, a.branch).Hash)
	require.Equal(t, bTip, headCommit(t, b.ri, b.branch).Hash)
}
