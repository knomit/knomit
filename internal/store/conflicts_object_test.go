package store

// Conflict merge PR 2: `conflicts` is an object {facts, state}. These pin the
// new values at every site that settles a conflict — the peer's sync, the
// rebase replay, MergePushed and MergeConsensus — and the consensus side each
// one names: the incoming consensus branch (src) at the peer's sync, onto
// (dst) in the replay, the host's own agent branch (dst) at the host.

import (
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// peerForkAgentConfident is the peer-sync shape where the confidence rule and
// the consensus rule disagree: the consensus branch ("master") rewrites the
// body and adds entity C; the agent rewrites the body MORE confidently and
// adds entity A. `merge` takes the agent's body, `merge:consensus` the
// consensus branch's, `consensus` the consensus branch's whole version.
func peerForkAgentConfident(t *testing.T, conflicts string) (*cmRepo, *object.Commit, *object.Commit) {
	r := newCMRepo(t)
	const consensus, agent = "master", "agent/peer-abcd1234"
	r.write("main", "kb/notes/f.md", cmFact(t, "base body", 0.7))
	r.branch(consensus, "main")
	r.branch(agent, "main")
	r.write(consensus, fact.OntologyFile, cmOntology(conflicts))
	r.write(consensus, "kb/notes/f.md", cmFact(t, "consensus body", 0.7, "C"))
	r.write(agent, "kb/notes/f.md", cmFact(t, "agent body", 0.9, "A"))
	r.setOriginTip(consensus)
	return r, r.tip(consensus), r.tip(agent)
}

func (r *cmRepo) peerSync() *object.Commit {
	r.t.Helper()
	const agent = "agent/peer-abcd1234"
	_, err := r.svc.Remote().(*remoteIndex).reconcileNow(context.Background(), agent, "master")
	require.NoError(r.t, err)
	return r.tip(agent)
}

// Peer sync, `facts: merge:consensus`: the consensus branch (src here) decides
// the body both changed, against the more confident agent; the agent's
// confidence (only it changed that) and both entities survive.
//
// SABOTAGE: the peer's consensus side inverted (mergeOpts.factMerge defaults
// to fact.MergeDst) → the agent's body → red.
func TestPeerSync_MergeConsensus_ConsensusBranchIsConsensusSide(t *testing.T) {
	r, _, _ := peerForkAgentConfident(t, "merge:consensus")
	tip := r.peerSync()
	_, out := r.blob(tip, "kb/notes/f.md")
	f := r.parse(out)
	require.Equal(t, "consensus body", f.Body, "the consensus side's body, although the agent is more confident")
	require.Equal(t, 0.9, f.Confidence, "only the agent changed the confidence: kept")
	require.Equal(t, []string{"C", "A"}, f.Entities, "both additions, the consensus side's first")
	lines := TrailerValues(tip.Message, TrailerMerge)
	require.Len(t, lines, 1, tip.Message)
	require.Contains(t, lines[0], "strategy=merge_consensus ")
	require.Contains(t, tip.Message, "(conflicts:merge:consensus/off)")
}

// Peer sync, `facts: merge` on the same fork: the confidence rule takes the
// agent's body — the contrast that makes the test above mean something.
func TestPeerSync_Merge_ConfidenceDecides(t *testing.T) {
	r, _, _ := peerForkAgentConfident(t, "merge")
	_, out := r.blob(r.peerSync(), "kb/notes/f.md")
	require.Equal(t, "agent body", r.parse(out).Body)
}

// Peer sync, `facts: consensus`: no field merge — the consensus branch's
// whole version, byte for byte, recorded as a consensus pick.
//
// SABOTAGE: the peer's consensus side inverted → the agent's bytes → red.
func TestPeerSync_FactsConsensus_WholeVersion(t *testing.T) {
	r, consensusTip, agentTip := peerForkAgentConfident(t, "consensus")
	tip := r.peerSync()
	_, out := r.blob(tip, "kb/notes/f.md")
	_, want := r.blob(consensusTip, "kb/notes/f.md")
	require.Equal(t, want, out, "the consensus branch's version, whole")
	require.Empty(t, TrailerValues(tip.Message, TrailerMerge))
	lines := TrailerValues(tip.Message, TrailerConflict)
	require.Len(t, lines, 1, tip.Message)
	require.Regexp(t, `^kb/notes/f\.md kept=src dropped=dst-modify strategy=consensus base=[0-9a-f]{40} src=[0-9a-f]{40} dst=[0-9a-f]{40}$`, lines[0])
	require.Equal(t, []plumbing.Hash{agentTip.Hash, consensusTip.Hash}, tip.ParentHashes, "the dropped version is one parent away")
}

// `facts: consensus` is a whole-version pick for a retraction too: the
// consensus side's deletion AND its edit both win — unlike the merge values,
// where a retraction always wins.
func TestPeerSync_FactsConsensus_ModifyDelete(t *testing.T) {
	r := newCMRepo(t)
	const consensus, agent = "master", "agent/peer-abcd1234"
	r.write("main", "kb/notes/d1.md", cmFact(t, "base", 0.7))
	r.write("main", "kb/notes/d2.md", cmFact(t, "base", 0.7))
	r.branch(consensus, "main")
	r.branch(agent, "main")
	r.write(consensus, fact.OntologyFile, cmOntology("consensus"))
	r.del(consensus, "kb/notes/d1.md") // consensus deletes, agent edits
	r.write(agent, "kb/notes/d1.md", cmFact(t, "agent edit", 0.7))
	consensusEdit := cmFact(t, "consensus edit", 0.7) // consensus edits, agent deletes
	r.write(consensus, "kb/notes/d2.md", consensusEdit)
	r.del(agent, "kb/notes/d2.md")
	r.setOriginTip(consensus)

	tip := r.peerSync()
	h, _ := r.blob(tip, "kb/notes/d1.md")
	require.True(t, h.IsZero(), "the consensus side's deletion wins")
	_, d2 := r.blob(tip, "kb/notes/d2.md")
	require.Equal(t, consensusEdit, d2, "the consensus side's edit wins over the agent's deletion")
	conflicts := strings.Join(TrailerValues(tip.Message, TrailerConflict), "\n")
	require.Regexp(t, `kb/notes/d1\.md kept=src dropped=dst-modify strategy=consensus `, conflicts)
	require.Regexp(t, `kb/notes/d2\.md kept=src dropped=dst-delete strategy=consensus `, conflicts)
}

// Peer sync, `state: consensus`: every path the fact merge does not take goes
// to the consensus branch's version, with the reason — a non-fact edit, a
// non-fact dual add, a fact the consensus side made unparsable, a fact the
// agent wrote with a frontmatter key this build does not know (lossy). A
// header comment is NOT lossy: that fact is merged.
//
// SABOTAGE: state read as off (stateRule → sitePick) → the agent's versions
// → red; comments lossy again → comment.md side-picked → red.
func TestPeerSync_StateConsensus(t *testing.T) {
	r := newCMRepo(t)
	const consensus, agent = "master", "agent/peer-abcd1234"
	r.write("main", "notes.txt", "base\n")
	r.write("main", "kb/notes/bad.md", cmFact(t, "base", 0.7))
	r.write("main", "kb/notes/lossy.md", cmFact(t, "base", 0.7))
	r.write("main", "kb/notes/comment.md", cmFact(t, "base", 0.7))
	r.branch(consensus, "main")
	r.branch(agent, "main")
	r.write(consensus, fact.OntologyFile, cmOntology("merge/consensus"))

	r.write(consensus, "notes.txt", "consensus\n")
	r.write(agent, "notes.txt", "agent\n")
	r.write(consensus, "extra.txt", "consensus added\n")
	r.write(agent, "extra.txt", "agent added\n")
	r.write(consensus, "kb/notes/bad.md", "not a fact at all\n")
	r.write(agent, "kb/notes/bad.md", cmFact(t, "agent", 0.7))
	consensusLossy := cmFact(t, "consensus", 0.7)
	r.write(consensus, "kb/notes/lossy.md", consensusLossy)
	lossy := strings.Replace(cmFact(t, "agent", 0.9), "---\n# ", "reviewed_by: alice\n---\n# ", 1)
	require.Contains(t, lossy, "reviewed_by: alice\n---\n")
	r.write(agent, "kb/notes/lossy.md", lossy)
	r.write(consensus, "kb/notes/comment.md", cmFact(t, "consensus", 0.7))
	commented := strings.Replace(cmFact(t, "base", 0.7, "Commented"), "confidence: 0.7", "confidence: 0.7 # checked by alice", 1)
	require.Contains(t, commented, "# checked by alice")
	r.write(agent, "kb/notes/comment.md", commented)
	r.setOriginTip(consensus)

	tip := r.peerSync()
	_, notes := r.blob(tip, "notes.txt")
	require.Equal(t, "consensus\n", notes, "not a fact: the consensus side's")
	_, extra := r.blob(tip, "extra.txt")
	require.Equal(t, "consensus added\n", extra, "a non-fact dual add: the consensus side's")
	_, bad := r.blob(tip, "kb/notes/bad.md")
	require.Equal(t, "not a fact at all\n", bad, "unmergeable: the consensus side's")
	_, gotLossy := r.blob(tip, "kb/notes/lossy.md")
	require.Equal(t, consensusLossy, gotLossy, "lossy: the consensus side's")
	_, merged := r.blob(tip, "kb/notes/comment.md")
	mf := r.parse(merged)
	require.Equal(t, "consensus", mf.Body, "the consensus side's body edit")
	require.Equal(t, []string{"Commented"}, mf.Entities, "and the commented version's entity: merged, not side-picked")
	require.NotContains(t, merged, "checked by alice", "the comment is dropped")

	conflicts := strings.Join(TrailerValues(tip.Message, TrailerConflict), "\n")
	require.Regexp(t, `notes\.txt kept=src dropped=dst-modify strategy=consensus .* reason=not-a-fact`, conflicts)
	require.Regexp(t, `extra\.txt kept=src dropped=dst-add strategy=consensus base=none .* reason=not-a-fact`, conflicts)
	require.Regexp(t, `kb/notes/bad\.md kept=src dropped=dst-modify strategy=consensus .* reason=src-unparsable`, conflicts)
	require.Regexp(t, `kb/notes/lossy\.md kept=src dropped=dst-modify strategy=consensus .* reason=dst-lossy`, conflicts)
	merges := TrailerValues(tip.Message, TrailerMerge)
	require.Len(t, merges, 1, tip.Message)
	require.True(t, strings.HasPrefix(merges[0], "kb/notes/comment.md strategy=merge "), merges[0])
	require.Contains(t, tip.Message, "(conflicts:merge/consensus)")
}

// `facts: off` with `state: consensus`: a fact keeps the site's own side-pick
// (the peer keeps its own, recorded without a reason) while a non-fact path
// takes the consensus side.
func TestPeerSync_FactsOffStateConsensus(t *testing.T) {
	r, _, agentTip := peerForkAgentConfident(t, "off/consensus")
	const consensus, agent = "master", "agent/peer-abcd1234"
	r.write("master", "notes.txt", "consensus\n")
	r.write(agent, "notes.txt", "agent\n")
	r.setOriginTip(consensus)
	agentTip = r.tip(agent)

	tip := r.peerSync()
	_, f := r.blob(tip, "kb/notes/f.md")
	_, agentF := r.blob(agentTip, "kb/notes/f.md")
	require.Equal(t, agentF, f, "facts: off — the peer keeps its own")
	_, notes := r.blob(tip, "notes.txt")
	require.Equal(t, "consensus\n", notes, "state: consensus — the consensus side's")
	conflicts := strings.Join(TrailerValues(tip.Message, TrailerConflict), "\n")
	require.Regexp(t, `kb/notes/f\.md kept=dst dropped=src-modify strategy=local_wins base=[0-9a-f]{40} src=[0-9a-f]{40} dst=[0-9a-f]{40}(\n|$)`, conflicts)
	require.Regexp(t, `notes\.txt kept=src dropped=dst-add strategy=consensus base=none .* reason=not-a-fact`, conflicts)
}

// `consensus: auto` alone, no `conflicts`: the peer's sync runs the default,
// facts: merge / state: consensus.
//
// SABOTAGE: drop the auto default → LocalWins → red.
func TestPeerSync_AutoDefault(t *testing.T) {
	r := newCMRepo(t)
	const consensus, agent = "master", "agent/peer-abcd1234"
	r.write("main", "kb/notes/f.md", cmFact(t, "base body", 0.7))
	r.write("main", "notes.txt", "base\n")
	r.branch(consensus, "main")
	r.branch(agent, "main")
	r.write(consensus, fact.OntologyFile, "id: x\nname: X\nattributes:\n  consensus: auto\ntopics:\n  notes:\n    description: d\n")
	r.write(consensus, "kb/notes/f.md", cmFact(t, "consensus body", 0.7, "C"))
	r.write(agent, "kb/notes/f.md", cmFact(t, "agent body", 0.9, "A"))
	r.write(consensus, "notes.txt", "consensus\n")
	r.write(agent, "notes.txt", "agent\n")
	r.setOriginTip(consensus)

	tip := r.peerSync()
	require.Contains(t, tip.Message, "(conflicts:merge/consensus)")
	_, out := r.blob(tip, "kb/notes/f.md")
	f := r.parse(out)
	require.Equal(t, "agent body", f.Body, "facts: merge — the more confident body")
	require.Equal(t, []string{"A", "C"}, f.Entities)
	_, notes := r.blob(tip, "notes.txt")
	require.Equal(t, "consensus\n", notes, "state: consensus")
}

// Host, `facts: consensus` at MergePushed and MergeConsensus: the host's own
// branch (dst) is the consensus side, so its whole version wins against a
// more confident peer, recorded.
//
// SABOTAGE: hostConflictStrategy's consensus side inverted (fact.MergeSrc) →
// the peer's bytes → red.
func TestHost_FactsConsensus_HostBranchWins(t *testing.T) {
	for _, site := range []string{"MergePushed", "MergeConsensus"} {
		t.Run(site, func(t *testing.T) {
			r, peerTip := hostFork(t, "consensus")
			hostBefore := r.tip(pmAgent)
			_, hostF := r.blob(hostBefore, "kb/notes/f.md")
			var err error
			if site == "MergePushed" {
				_, err = r.svc.MergePushed(context.Background(), pmPeer, pmAgent, peerTip, "")
			} else {
				_, err = r.svc.MergeConsensus(context.Background(), pmPeer, pmAgent, peerTip)
			}
			require.NoError(t, err, "settled, not refused")
			tip := r.tip(pmAgent)
			require.Equal(t, []plumbing.Hash{hostBefore.Hash, peerTip}, tip.ParentHashes)
			_, out := r.blob(tip, "kb/notes/f.md")
			require.Equal(t, hostF, out, "the host's whole version")
			require.Regexp(t, `^kb/notes/f\.md kept=dst dropped=src-modify strategy=consensus `,
				strings.Join(TrailerValues(tip.Message, TrailerConflict), "\n"))
			require.Contains(t, tip.Message, "(conflicts:consensus/off)")
		})
	}
}

// Host, `facts: merge:consensus` at MergeConsensus (MergePushed has its own
// test): the fields both changed — body and confidence — take the host's
// values against a more confident peer; the host's entity is kept.
func TestMergeConsensus_MergeConsensus_HostIsConsensus(t *testing.T) {
	r, peerTip := hostFork(t, "merge:consensus")
	_, err := r.svc.MergeConsensus(context.Background(), pmPeer, pmAgent, peerTip)
	require.NoError(t, err)
	tip := r.tip(pmAgent)
	_, out := r.blob(tip, "kb/notes/f.md")
	f := r.parse(out)
	require.Equal(t, "host body", f.Body)
	require.Equal(t, 0.6, f.Confidence)
	require.Equal(t, []string{"HostAdded"}, f.Entities)
	require.Contains(t, TrailerValues(tip.Message, TrailerMerge)[0], "strategy=merge_consensus ")
}

// Host, `state: consensus`: a non-fact both changed no longer refuses the
// merge — the host's version wins — and a fact the peer made lossy takes the
// host's version too. Compare TestMergePushed_MergeFacts_UnmergeableStillRefused
// (state off).
func TestHost_StateConsensus_NotRefused(t *testing.T) {
	for _, site := range []string{"MergePushed", "MergeConsensus"} {
		t.Run(site, func(t *testing.T) {
			r, _ := hostFork(t, "merge/consensus")
			r.write(pmAgent, "notes.txt", "host\n")
			r.write(pmPeer, "notes.txt", "peer\n")
			hostLossy := cmFact(t, "host", 0.7)
			r.write(pmAgent, "kb/notes/lossy.md", hostLossy)
			peerTip := r.write(pmPeer, "kb/notes/lossy.md", strings.Replace(cmFact(t, "peer", 0.9), "---\n# ", "reviewed_by: bob\n---\n# ", 1))
			var err error
			if site == "MergePushed" {
				_, err = r.svc.MergePushed(context.Background(), pmPeer, pmAgent, peerTip, "")
			} else {
				_, err = r.svc.MergeConsensus(context.Background(), pmPeer, pmAgent, peerTip)
			}
			require.NoError(t, err)
			tip := r.tip(pmAgent)
			_, notes := r.blob(tip, "notes.txt")
			require.Equal(t, "host\n", notes)
			_, lossy := r.blob(tip, "kb/notes/lossy.md")
			require.Equal(t, hostLossy, lossy)
			conflicts := strings.Join(TrailerValues(tip.Message, TrailerConflict), "\n")
			require.Regexp(t, `notes\.txt kept=dst dropped=src-add strategy=consensus base=none .* reason=not-a-fact`, conflicts)
			require.Regexp(t, `kb/notes/lossy\.md kept=dst dropped=src-add strategy=consensus base=none .* reason=src-lossy`, conflicts)
			require.Len(t, TrailerValues(tip.Message, TrailerMerge), 1, "f.md merged")
		})
	}
}

// Replay, `facts: consensus`: onto (dst) is the consensus side — its whole
// version wins over the more confident agent commit.
//
// SABOTAGE: replayCommit's consensus side inverted (fact.MergeSrc) → the
// agent's bytes → red.
func TestReplay_FactsConsensus_OntoWins(t *testing.T) {
	r, agent, onto := replayFork(t)
	ontoCommit, err := object.GetCommit(r.svc.rh.gits, onto)
	require.NoError(t, err)
	_, ontoF := r.blob(ontoCommit, "kb/notes/f.md")
	_, err = r.svc.rh.reconcileAgent(context.Background(), agent, "main", stratConsensus, true)
	require.NoError(t, err)
	tip := r.tip(agent)
	_, out := r.blob(tip, "kb/notes/f.md")
	require.Equal(t, ontoF, out, "onto's whole version")
	require.Regexp(t, `kb/notes/f\.md kept=dst dropped=src-modify strategy=consensus `,
		strings.Join(TrailerValues(tip.Message, TrailerConflict), "\n"), tip.Message)
}

// Replay, `state: consensus`: a non-fact both changed goes to onto, not to
// the agent's commit as the replay's own rule would have it.
func TestReplay_StateConsensus_OntoWins(t *testing.T) {
	r, agent, _ := replayFork(t)
	r.write("main", "notes.txt", "main\n")
	r.write(agent, "notes.txt", "agent\n")
	_, err := r.svc.rh.reconcileAgent(context.Background(), agent, "main", stratMergeState, true)
	require.NoError(t, err)
	tip := r.tip(agent)
	_, notes := r.blob(tip, "notes.txt")
	require.Equal(t, "main\n", notes)
	require.Regexp(t, `notes\.txt kept=dst dropped=src-modify strategy=consensus .* reason=not-a-fact`,
		strings.Join(TrailerValues(tip.Message, TrailerConflict), "\n"), tip.Message)
}

// The policy round-trips through its strategy value, and nothing else parses
// as one.
func TestConflictsPolicy_StrategyRoundTrip(t *testing.T) {
	for _, facts := range []string{fact.ConflictsOff, fact.ConflictsMerge, fact.ConflictsMergeConsensus, fact.ConflictsConsensus} {
		for _, state := range []string{fact.ConflictsOff, fact.ConflictsConsensus} {
			p := conflictsPolicy{facts: facts, state: state}
			got, ok := conflictsPolicyOf(p.strategy())
			require.True(t, ok, p.strategy())
			require.Equal(t, p, got)
		}
	}
	for _, s := range []ConflictStrategy{StrategyLocalWins, StrategyRemoteWins, StrategyRefuse, "", "conflicts:", "conflicts:merge",
		"conflicts:merge/merge:consensus", "conflicts:bogus/off", "merge_facts"} {
		_, ok := conflictsPolicyOf(s)
		require.False(t, ok, s)
	}
}
