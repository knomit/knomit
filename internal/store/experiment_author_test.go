package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
)

// #394: a commit written on an experiment branch is authored and committed by
// the agent that OWNS the experiment (its recorded parent, the agent branch it
// was forked from) — the identity whose key signs it — never `exp/<name>`.

// commitsSince lists the commits reachable from tip and not from stop, newest
// first.
func commitsSince(t *testing.T, svc *Service, tip, stop string) []*object.Commit {
	t.Helper()
	seen := map[plumbing.Hash]bool{}
	if stop != "" {
		stopC, err := svc.rh.repo.CommitObject(plumbing.NewHash(stop))
		require.NoError(t, err)
		require.NoError(t, object.NewCommitPreorderIter(stopC, nil, nil).ForEach(func(c *object.Commit) error {
			seen[c.Hash] = true
			return nil
		}))
	}
	tipC, err := svc.rh.repo.CommitObject(plumbing.NewHash(tip))
	require.NoError(t, err)
	var out []*object.Commit
	require.NoError(t, object.NewCommitPreorderIter(tipC, seen, nil).ForEach(func(c *object.Commit) error {
		out = append(out, c)
		return nil
	}))
	return out
}

// requireAuthoredBy asserts the author and committer of every commit are the
// agent id, with the operation sub-address on the author.
func requireAuthoredBy(t *testing.T, cs []*object.Commit, id string) {
	t.Helper()
	for _, c := range cs {
		subject := strings.SplitN(c.Message, "\n", 2)[0]
		require.Equal(t, id, c.Author.Name, "author of %q", subject)
		require.True(t, strings.HasPrefix(c.Author.Email, id+"+"), "author email of %q: %s", subject, c.Author.Email)
		require.True(t, strings.HasSuffix(c.Author.Email, "@agents.knomit.io"), "author email of %q: %s", subject, c.Author.Email)
		require.Equal(t, id, c.Committer.Name, "committer of %q", subject)
		require.Equal(t, id+"@agents.knomit.io", c.Committer.Email, "committer email of %q", subject)
		require.NotContains(t, c.Author.Email, "exp/", "never the experiment's name: %q", subject)
	}
}

// TestExperimentCommits_AuthoredByOwningAgent drives every write path onto an
// experiment — a single write (learn, update), a delete (retract), a batch (an
// atomic move: the path review, hypothesize and learn-move use), the sync
// merge commit — then commits it twice: once as a fast-forward and once as a
// merge commit. Every commit that reaches the agent branch is authored and
// committed by the owning agent (`test`, from agent/test). Sabotage: resolve
// the identity from the branch written (the pre-#394 deriveAgentID(branch))
// — red: `exp/…` authors.
func TestExperimentCommits_AuthoredByOwningAgent(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)
	svc.SetSigner(newTestSigner(t))
	require.NoError(t, svc.Branches().SetAgentBranchOwner(ctx, testAgentBranch), "a KNOWN owner, equal to the recorded parent")
	forkBase, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)

	// Experiment 1: every write kind, a sync merge, then a merge-commit commit.
	_, err = svc.Experiments().OpenExperiment(ctx, "every-path", "", testAgentBranch)
	require.NoError(t, err)
	exp := "exp/every-path"
	content := func(title string) string { return "---\ntype: observation\n---\n# " + title + "\n\nbody\n" }
	_, err = svc.Facts().WriteFact(ctx, exp, "kb/a.md", content("a"), "learn: a", "learn")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, exp, "kb/a.md", content("a v2"), "update(x): a", "update")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, exp, "kb/gone.md", content("gone"), "learn: gone", "learn")
	require.NoError(t, err)
	_, err = svc.Facts().DeleteFact(ctx, exp, "kb/gone.md", "retract(x): kb/gone.md")
	require.NoError(t, err)
	_, _, err = svc.Facts().BatchWriteFacts(ctx, exp, map[string]string{"kb/b.md": content("b")}, []string{"kb/a.md"}, "move: b", "move")
	require.NoError(t, err)
	// The agent branch moves, so sync writes a merge commit ON the experiment.
	writeMergeFact(t, svc, testAgentBranch, "kb/agent-side.md", "agent side", "body")
	syncRes, err := svc.Experiments().SyncExperiment(ctx, "every-path")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, syncRes.Mode, "fixture: sync must synthesize a merge commit on the experiment")
	expTip, err := svc.Branches().HeadCommit(ctx, exp)
	require.NoError(t, err)
	onExp := commitsSince(t, svc, expTip, forkBase)
	require.GreaterOrEqual(t, len(onExp), 6, "fixture: five writes and the sync merge (plus the agent-side write) are on the experiment")
	requireAuthoredBy(t, onExp, "test")

	// Then the agent branch moves again, so the commit is a merge commit.
	writeMergeFact(t, svc, testAgentBranch, "kb/agent-side-2.md", "agent side 2", "body")
	res, err := svc.Experiments().CommitExperiment(ctx, "every-path", nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode, "fixture: the commit writes a merge commit")

	// Experiment 2: a fast-forward commit carries the experiment's own commits
	// onto the agent branch unchanged — so they must already be the agent's.
	_, err = svc.Experiments().OpenExperiment(ctx, "fast-forward", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/fast-forward", "kb/ff.md", "ff", "body")
	res, err = svc.Experiments().CommitExperiment(ctx, "fast-forward", nil)
	require.NoError(t, err)
	require.Equal(t, ModeFF, res.Mode, "fixture: nothing moved on the parent, so this is a fast-forward")

	agentTip, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	all := commitsSince(t, svc, agentTip, forkBase)
	require.GreaterOrEqual(t, len(all), 10, "fixture: both experiments' commits are on the agent branch")
	requireAuthoredBy(t, all, "test")
}

// TestExperimentCommits_EmptyOwnerUsesRecordedParent: a database that has not
// recorded its owner (pre-key, never booted) is UNKNOWN, not a mismatch — the
// recorded parent names the agent (kb/invariants/store/experiments/parent-vs-owner).
func TestExperimentCommits_EmptyOwnerUsesRecordedParent(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)
	owner, err := svc.Branches().AgentBranchOwner(ctx)
	require.NoError(t, err)
	require.Empty(t, owner, "fixture: no owner recorded")

	_, err = svc.Experiments().OpenExperiment(ctx, "unowned", "", testAgentBranch)
	require.NoError(t, err)
	h := writeMergeFact(t, svc, "exp/unowned", "kb/x.md", "x", "body")
	c, err := svc.rh.repo.CommitObject(plumbing.NewHash(h))
	require.NoError(t, err)
	requireAuthoredBy(t, []*object.Commit{c}, "test")
}

// TestExperimentCommits_NoOwnerNoWrite: when the owning agent cannot be named,
// the write is REFUSED and nothing moves — never authored `exp/<name>`:
// an orphan ref (no experiments row), and a recorded parent that is no longer
// this database's known owner (taken over since the fork).
func TestExperimentCommits_NoOwnerNoWrite(t *testing.T) {
	ctx := context.Background()
	content := "---\ntype: observation\n---\n# x\n\nbody\n"

	t.Run("orphan ref", func(t *testing.T) {
		svc := newExperimentTestStore(t)
		require.NoError(t, svc.Branches().CreateBranch(ctx, "exp/ghost", testAgentBranch))
		before, err := svc.Branches().HeadCommit(ctx, "exp/ghost")
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, "exp/ghost", "kb/x.md", content, "learn: x", "learn")
		require.ErrorIs(t, err, ErrOrphanExperimentRef)
		_, err = svc.Facts().DeleteFact(ctx, "exp/ghost", "kb/base.md", "retract(x): kb/base.md")
		require.ErrorIs(t, err, ErrOrphanExperimentRef)
		_, _, err = svc.Facts().BatchWriteFacts(ctx, "exp/ghost", map[string]string{"kb/y.md": content}, nil, "move: y", "move")
		require.ErrorIs(t, err, ErrOrphanExperimentRef)
		after, err := svc.Branches().HeadCommit(ctx, "exp/ghost")
		require.NoError(t, err)
		require.Equal(t, before, after, "a refused write moves nothing")
	})

	t.Run("taken over", func(t *testing.T) {
		svc := newExperimentTestStore(t)
		_, err := svc.Experiments().OpenExperiment(ctx, "stale", "", testAgentBranch)
		require.NoError(t, err)
		writeMergeFact(t, svc, testAgentBranch, "kb/moved.md", "moved", "body")
		require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/successor", testAgentBranch))
		require.NoError(t, svc.Branches().SetAgentBranchOwner(ctx, "agent/successor"))
		before, err := svc.Branches().HeadCommit(ctx, "exp/stale")
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, "exp/stale", "kb/x.md", content, "learn: x", "learn")
		require.ErrorIs(t, err, ErrStaleExperimentParent)
		// The sync merge commit is refused the same way (its own gate first).
		_, err = svc.Experiments().SyncExperiment(ctx, "stale")
		require.ErrorIs(t, err, ErrStaleExperimentParent)
		after, err := svc.Branches().HeadCommit(ctx, "exp/stale")
		require.NoError(t, err)
		require.Equal(t, before, after, "a refused write moves nothing")
	})

	t.Run("merge commit onto an orphan ref", func(t *testing.T) {
		// mergeIntoBranch resolves the author BEFORE writing any tree: an
		// experiment whose owner cannot be named gets no merge commit. Both
		// sides moved, so this is a real merge, not a fast-forward.
		svc := newExperimentTestStore(t)
		require.NoError(t, svc.Branches().CreateBranch(ctx, "exp/ghost", testAgentBranch))
		writeOnOrphanRef(t, svc, "ghost", "kb/ghost.md", "ghost", "body")
		writeMergeFact(t, svc, testAgentBranch, "kb/moved.md", "moved", "body")
		before, err := svc.Branches().HeadCommit(ctx, "exp/ghost")
		require.NoError(t, err)
		_, err = svc.rh.mergeIntoBranch(ctx, testAgentBranch, "exp/ghost", StrategyRemoteWins)
		require.ErrorIs(t, err, ErrOrphanExperimentRef)
		after, err := svc.Branches().HeadCommit(ctx, "exp/ghost")
		require.NoError(t, err)
		require.Equal(t, before, after)
	})
}

// copyToBare copies every object reachable from the given commits out of the
// store into a bare repository on disk and points the named branches at them,
// so the acceptance gate (CheckRange) reads the store's commits exactly as a CI
// job reads a pushed KB.
func copyToBare(t *testing.T, svc *Service, refs map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "kb.git")
	bare, err := gogit.PlainInit(dir, true)
	require.NoError(t, err)
	var tips []plumbing.Hash
	for _, h := range refs {
		tips = append(tips, plumbing.NewHash(h))
	}
	hashes, err := revlist.Objects(svc.rh.repo.Storer, tips, nil)
	require.NoError(t, err)
	for _, h := range hashes {
		o, err := svc.rh.repo.Storer.EncodedObject(plumbing.AnyObject, h)
		require.NoError(t, err)
		_, err = bare.Storer.SetEncodedObject(o)
		require.NoError(t, err)
	}
	for name, h := range refs {
		require.NoError(t, bare.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), plumbing.NewHash(h))))
	}
	return dir
}

// TestCheckRange_ExperimentRangeNamesTheOwningAgent: the acceptance gate in
// ENFORCE, over a candidate written through an experiment (a learn, a sync
// merge and a merge-commit commit), finds the OWNING agent's member record for
// every commit and admits the range. Before #394 each experiment commit was
// authored `exp/<name>` and refused RuleNoRecord ("no member record for agent
// exp/…"). Sabotage: as above — red: RuleNoRecord on the experiment commits.
func TestCheckRange_ExperimentRangeNamesTheOwningAgent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{fact.OntologyFile: kbOntology(VerifyEnforce)}, "main"))
	signer := newTestSigner(t)
	svc.SetSigner(signer)
	require.NoError(t, svc.Branches().CreateBranch(ctx, testAgentBranch, "main"))
	require.NoError(t, svc.Branches().SetAgentBranchOwner(ctx, testAgentBranch))

	_, err = svc.Experiments().OpenExperiment(ctx, "gated", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/gated", "kb/notes/one.md", "one", "body")
	writeMergeFact(t, svc, testAgentBranch, "kb/notes/agent.md", "agent", "body")
	_, err = svc.Experiments().SyncExperiment(ctx, "gated")
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/gated", "kb/notes/two.md", "two", "body")
	writeMergeFact(t, svc, testAgentBranch, "kb/notes/agent-2.md", "agent 2", "body")
	res, err := svc.Experiments().CommitExperiment(ctx, "gated", nil)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode, "fixture: a merge commit lands the experiment")

	mainTip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	agentTip, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	kbDir := copyToBare(t, svc, map[string]string{"main": mainTip, "cand": agentTip})

	f := newRangeFixture(t)
	f.setFleet(map[string]string{"test/m.md": memberFile("test", fact.MemberActive, signer.PublicKey().(ssh.PublicKey))})
	v, err := CheckRange(RangeInput{KBDir: kbDir, Main: "main", Candidate: "cand", FleetDir: f.fleetDir, FleetRev: "main"})
	require.NoError(t, err)
	require.Equal(t, VerifyEnforce, v.Mode, "fixture: main's ontology enforces")
	require.Equal(t, 6, v.Checked, "fixture: the experiment's two writes, the agent's two, the sync merge and the commit merge are judged")
	require.Empty(t, v.Refused, "every commit names the owning agent, whose current key signed it")
	require.Equal(t, 0, v.ExitCode())
}
