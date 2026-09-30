package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// cmFact is a fact version for the conflict-merge tests.
func cmFact(t *testing.T, body string, conf float64, entities ...string) string {
	t.Helper()
	f := fact.NewFact("kb/notes/f.md")
	f.Title, f.Body = "Shared fact", body
	f.Kind, f.Type = fact.Epistemic, fact.Observation
	f.Confidence, f.Sources = conf, 1
	f.Domain, f.Refs = []string{"store"}, []string{}
	f.Entities = append([]string{}, entities...)
	s, err := fact.SerializeFact(f)
	require.NoError(t, err)
	return s
}

func cmOntology(conflicts string) string {
	s := "id: x\nname: X\n"
	if conflicts != "" {
		s += "attributes:\n  conflicts: " + conflicts + "\n"
	}
	return s + "topics:\n  notes:\n    description: d\n"
}

type cmRepo struct {
	t   *testing.T
	svc *Service
}

func newCMRepo(t *testing.T) *cmRepo {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{fact.OntologyFile: cmOntology("")}, "agent/seed"))
	return &cmRepo{t: t, svc: svc}
}

func (r *cmRepo) write(branch, path, content string) plumbing.Hash {
	r.t.Helper()
	res, err := r.svc.Facts().WriteFact(context.Background(), branch, path, content, "learn: "+path, "learn")
	require.NoError(r.t, err)
	return plumbing.NewHash(res.CommitHash)
}

func (r *cmRepo) del(branch, path string) {
	r.t.Helper()
	_, err := r.svc.Facts().DeleteFact(context.Background(), branch, path, "retract: "+path)
	require.NoError(r.t, err)
}

func (r *cmRepo) branch(name, from string) {
	r.t.Helper()
	require.NoError(r.t, r.svc.Branches().CreateBranch(context.Background(), name, from))
}

func (r *cmRepo) tip(branch string) *object.Commit {
	r.t.Helper()
	ref, err := r.svc.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	require.NoError(r.t, err)
	c, err := object.GetCommit(r.svc.rh.gits, ref.Hash())
	require.NoError(r.t, err)
	return c
}

// blob is path's blob at c (zero when absent) and its content.
func (r *cmRepo) blob(c *object.Commit, path string) (plumbing.Hash, string) {
	r.t.Helper()
	f, err := c.File(path)
	if errors.Is(err, object.ErrFileNotFound) {
		return plumbing.ZeroHash, ""
	}
	require.NoError(r.t, err)
	s, err := f.Contents()
	require.NoError(r.t, err)
	return f.Hash, s
}

func (r *cmRepo) parse(content string) fact.Fact {
	r.t.Helper()
	f, err := fact.ParseFact("kb/notes/f.md", content)
	require.NoError(r.t, err)
	return f
}

// setOriginTip makes refs/remotes/origin/<branch> equal the local branch, the
// state a fetch leaves, so reconcileNow's main step is a no-op.
func (r *cmRepo) setOriginTip(branch string) {
	r.t.Helper()
	require.NoError(r.t, r.svc.rh.gits.SetReference(plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", branch), r.tip(branch).Hash)))
}

func mergeLine(path string, base, src, dst, out plumbing.Hash, decided string) string {
	return fmt.Sprintf("%s strategy=confidence base=%s src=%s dst=%s out=%s decided=%s",
		path, blobName(base), blobName(src), blobName(dst), blobName(out), decided)
}

// peerFork is the peer-sync shape shared by the tests below: the fact is on
// both the consensus branch (`consensus`, a name that is NOT "main") and the
// agent branch, then both change it. The consensus side is more confident and
// rewrites the body; the agent adds an entity and rewrites the body too. The
// consensus branch also gains an unrelated fact, so a merge that drops its F
// change still writes a merge commit (probe case A).
func peerFork(t *testing.T, consensusOnt, agentOnt string) (*cmRepo, *object.Commit, *object.Commit, *object.Commit) {
	r := newCMRepo(t)
	const consensus, agent = "master", "agent/peer-abcd1234"
	baseCommit := r.write("main", "kb/notes/f.md", cmFact(t, "base body", 0.7))
	r.branch(consensus, "main")
	r.branch(agent, "main")
	if consensusOnt != "" {
		r.write(consensus, fact.OntologyFile, consensusOnt)
	}
	if agentOnt != "" {
		r.write(agent, fact.OntologyFile, agentOnt)
	}
	r.write(consensus, "kb/notes/f.md", cmFact(t, "consensus body", 0.9))
	r.write(consensus, "kb/notes/g.md", cmFact(t, "unrelated", 0.7))
	r.write(agent, "kb/notes/f.md", cmFact(t, "agent body", 0.7, "AgentAdded"))
	r.setOriginTip(consensus)
	base, err := object.GetCommit(r.svc.rh.gits, baseCommit)
	require.NoError(t, err)
	return r, base, r.tip(consensus), r.tip(agent)
}

// A peer's sync with `conflicts: merge` at the tip of its consensus branch —
// here "master", so a hardcoded "main" reads nothing — merges the fact both
// sides changed: the more confident consensus body, the agent's added entity,
// one Knomit-Merge line naming the four real blobs.
//
// SABOTAGE: keep StrategyLocalWins at remote_sync.go's reconcileAgent call
// (S12a), read the attribute at "main" (S9) or at the agent branch (S8), or
// drop the trailer (S10a) → red.
func TestPeerSync_MergeFacts_NonMainConsensus(t *testing.T) {
	r, base, consensusTip, agentTip := peerFork(t, cmOntology("merge"), "")
	const agent = "agent/peer-abcd1234"

	res, err := r.svc.Remote().(*remoteIndex).reconcileNow(context.Background(), agent, "master")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Agent.Mode)

	tip := r.tip(agent)
	require.Equal(t, []plumbing.Hash{agentTip.Hash, consensusTip.Hash}, tip.ParentHashes)
	outBlob, out := r.blob(tip, "kb/notes/f.md")
	f := r.parse(out)
	require.Equal(t, "consensus body", f.Body, "both changed the body: the confident version's, whole")
	require.Equal(t, 0.9, f.Confidence)
	require.Equal(t, []string{"AgentAdded"}, f.Entities, "only the agent changed entities: kept")

	baseBlob, _ := r.blob(base, "kb/notes/f.md")
	srcBlob, _ := r.blob(consensusTip, "kb/notes/f.md")
	dstBlob, _ := r.blob(agentTip, "kb/notes/f.md")
	require.True(t, strings.HasPrefix(tip.Message, "merge: master into "+agent+" (merge_facts)\n"), tip.Message)
	require.Equal(t, []string{mergeLine("kb/notes/f.md", baseBlob, srcBlob, dstBlob, outBlob, "body")},
		TrailerValues(tip.Message, TrailerMerge), "message:\n%s", tip.Message)
	require.Empty(t, TrailerValues(tip.Message, TrailerConflict))
	verifyMergeClean(t, r.svc, agent)
}

// The setting is read at the CONSENSUS branch's tip only. The same fork with
// `conflicts: merge` on the agent branch alone is today's sync: LocalWins
// keeps the agent's F — and the dropped consensus edit is now RECORDED.
//
// SABOTAGE: read the attribute at the agent branch (S8) → merged → red;
// default an absent setting to merge (S7) → red; drop the walk's
// Knomit-Conflict line (S10b) → red.
func TestPeerSync_SettingOnAgentBranchOnly_IsToday(t *testing.T) {
	r, base, consensusTip, agentTip := peerFork(t, "", cmOntology("merge"))
	const agent = "agent/peer-abcd1234"

	res, err := r.svc.Remote().(*remoteIndex).reconcileNow(context.Background(), agent, "master")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Agent.Mode)

	tip := r.tip(agent)
	_, out := r.blob(tip, "kb/notes/f.md")
	_, agentF := r.blob(agentTip, "kb/notes/f.md")
	require.Equal(t, agentF, out, "LocalWins: the agent's version, byte for byte")
	require.True(t, strings.HasPrefix(tip.Message, "merge: master into "+agent+" (local_wins)\n"), tip.Message)
	baseBlob, _ := r.blob(base, "kb/notes/f.md")
	srcBlob, _ := r.blob(consensusTip, "kb/notes/f.md")
	dstBlob, _ := r.blob(agentTip, "kb/notes/f.md")
	require.Equal(t, []string{fmt.Sprintf("kb/notes/f.md kept=dst dropped=src-modify strategy=local_wins base=%s src=%s dst=%s",
		baseBlob, srcBlob, dstBlob)}, TrailerValues(tip.Message, TrailerConflict), "message:\n%s", tip.Message)
	require.Empty(t, TrailerValues(tip.Message, TrailerMerge))
}

// What the merge cannot merge keeps the peer's side-pick and says why; a fact
// one side deleted stays deleted whichever side it was (ruling 2), with the
// dropped edit named.
//
// SABOTAGE: ok=true for a non-fact path (skip isFactPath) → notes.txt gets
// fact-merged / errors → red; resurrect on modify/delete (edit wins) → d1 or
// d2 present → red.
func TestPeerSync_MergeFacts_FallbackAndRetraction(t *testing.T) {
	r := newCMRepo(t)
	const consensus, agent = "master", "agent/peer-abcd1234"
	r.write("main", "kb/notes/f.md", cmFact(t, "base", 0.7))
	r.write("main", "notes.txt", "base\n")
	r.write("main", "kb/notes/bad.md", cmFact(t, "base", 0.7))
	r.write("main", "kb/notes/d1.md", cmFact(t, "base", 0.7))
	r.write("main", "kb/notes/d2.md", cmFact(t, "base", 0.7))
	r.branch(consensus, "main")
	r.branch(agent, "main")
	r.write(consensus, fact.OntologyFile, cmOntology("merge"))

	r.write(consensus, "kb/notes/f.md", cmFact(t, "consensus", 0.9))
	r.write(agent, "kb/notes/f.md", cmFact(t, "agent", 0.5))
	r.write(consensus, "notes.txt", "consensus\n")
	r.write(agent, "notes.txt", "agent\n")
	r.write(consensus, "kb/notes/bad.md", "not a fact at all\n")
	r.write(agent, "kb/notes/bad.md", cmFact(t, "agent", 0.7))
	r.write(consensus, "extra.txt", "consensus added\n") // dual add, not a fact
	r.write(agent, "extra.txt", "agent added\n")
	r.del(consensus, "kb/notes/d1.md") // src deletes, dst edits
	r.write(agent, "kb/notes/d1.md", cmFact(t, "agent edit", 0.7))
	r.write(consensus, "kb/notes/d2.md", cmFact(t, "consensus edit", 0.7)) // src edits, dst deletes
	r.del(agent, "kb/notes/d2.md")
	r.setOriginTip(consensus)
	agentBefore := r.tip(agent)

	_, err := r.svc.Remote().(*remoteIndex).reconcileNow(context.Background(), agent, consensus)
	require.NoError(t, err)
	tip := r.tip(agent)

	_, f := r.blob(tip, "kb/notes/f.md")
	require.Equal(t, "consensus", r.parse(f).Body)
	_, notes := r.blob(tip, "notes.txt")
	require.Equal(t, "agent\n", notes, "not a fact: LocalWins keeps the agent's")
	_, bad := r.blob(tip, "kb/notes/bad.md")
	_, agentBad := r.blob(agentBefore, "kb/notes/bad.md")
	require.Equal(t, agentBad, bad, "unparsable: LocalWins keeps the agent's")
	_, extra := r.blob(tip, "extra.txt")
	require.Equal(t, "consensus added\n", extra, "a non-fact dual add keeps today's LocalWins overwrite")
	for _, p := range []string{"kb/notes/d1.md", "kb/notes/d2.md"} {
		h, _ := r.blob(tip, p)
		require.True(t, h.IsZero(), "%s: the retraction wins", p)
	}

	merges := strings.Join(TrailerValues(tip.Message, TrailerMerge), "\n")
	conflicts := strings.Join(TrailerValues(tip.Message, TrailerConflict), "\n")
	require.Contains(t, merges, "kb/notes/f.md strategy=confidence ")
	require.Regexp(t, `kb/notes/d1\.md strategy=confidence base=[0-9a-f]{40} src=none dst=[0-9a-f]{40} out=none dropped=dst-modify decided=delete`, merges)
	require.Regexp(t, `kb/notes/d2\.md strategy=confidence base=[0-9a-f]{40} src=[0-9a-f]{40} dst=none out=none dropped=src-modify decided=delete`, merges)
	require.Regexp(t, `notes\.txt kept=dst dropped=src-modify strategy=local_wins .* reason=not-a-fact`, conflicts)
	require.Regexp(t, `kb/notes/bad\.md kept=dst dropped=src-modify strategy=local_wins .* reason=src-unparsable`, conflicts)
	require.Regexp(t, `extra\.txt kept=src dropped=dst-add strategy=local_wins base=none .* reason=not-a-fact`, conflicts)
	// The dropped edits are one parent away.
	h, _ := r.blob(agentBefore, "kb/notes/d1.md")
	require.False(t, h.IsZero(), "the agent's edit of d1 stays in the first parent")
}

// hostFork is the host shape: this instance's agent branch and a peer's pushed
// branch both change F. The host's consensus branch is "master" (its origin
// row says so), and main carries no setting.
func hostFork(t *testing.T, conflicts string) (*cmRepo, plumbing.Hash) {
	r := newCMRepo(t)
	r.svc.SetOrigin(&Origin{URL: "https://example.invalid/kb.git", Branch: "master"})
	require.Equal(t, "master", r.svc.UpstreamBranch())
	r.write("main", "kb/notes/f.md", cmFact(t, "base", 0.7))
	r.branch("master", "main")
	r.write("master", fact.OntologyFile, cmOntology(conflicts))
	r.branch(pmAgent, "master")
	r.branch(pmPeer, "main")
	r.write(pmAgent, "kb/notes/f.md", cmFact(t, "host body", 0.6, "HostAdded"))
	peerTip := r.write(pmPeer, "kb/notes/f.md", cmFact(t, "peer body", 0.8))
	return r, peerTip
}

// The host's merge of a peer's branch reads `conflicts` at ITS consensus
// branch (UpstreamBranch = "master" here) and merges the fact: the peer's more
// confident body, the host's entity, the trailer on the host's merge commit.
//
// SABOTAGE: MergePushed ignoring the attribute (S12c) → refused → red; reading
// it at "main" → refused → red.
func TestMergePushed_MergeFacts_NonMainUpstream(t *testing.T) {
	r, peerTip := hostFork(t, "merge")
	hostBefore := r.tip(pmAgent)

	res, err := r.svc.MergePushed(context.Background(), pmPeer, pmAgent, peerTip, "")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode)
	tip := r.tip(pmAgent)
	require.Equal(t, []plumbing.Hash{hostBefore.Hash, peerTip}, tip.ParentHashes)
	_, out := r.blob(tip, "kb/notes/f.md")
	f := r.parse(out)
	require.Equal(t, "peer body", f.Body)
	require.Equal(t, 0.8, f.Confidence)
	require.Equal(t, []string{"HostAdded"}, f.Entities)
	require.Len(t, TrailerValues(tip.Message, TrailerMerge), 1, tip.Message)
	require.Contains(t, tip.Message, "(merge_facts)")
}

// merge:upstream on the host: the host's branch is the upstream side, so a
// field both changed takes the host's value even against a more confident
// peer.
func TestMergePushed_MergeFactsUpstream_HostIsUpstream(t *testing.T) {
	r, peerTip := hostFork(t, "merge:upstream")
	_, err := r.svc.MergePushed(context.Background(), pmPeer, pmAgent, peerTip, "")
	require.NoError(t, err)
	tip := r.tip(pmAgent)
	_, out := r.blob(tip, "kb/notes/f.md")
	require.Equal(t, "host body", r.parse(out).Body)
	require.Contains(t, TrailerValues(tip.Message, TrailerMerge)[0], "strategy=upstream ")
	require.Contains(t, tip.Message, "(merge_facts_upstream)")
}

// An explicit side is a human's whole-set choice: the setting never overrides
// it, and the choice is recorded per path.
func TestMergePushed_ExplicitSideBeatsSetting(t *testing.T) {
	r, peerTip := hostFork(t, "merge")
	_, err := r.svc.MergePushed(context.Background(), pmPeer, pmAgent, peerTip, ResolveSrc)
	require.NoError(t, err)
	tip := r.tip(pmAgent)
	_, out := r.blob(tip, "kb/notes/f.md")
	_, peerF := r.blob(r.tip(pmPeer), "kb/notes/f.md")
	require.Equal(t, peerF, out, "the peer's version, whole")
	require.Empty(t, TrailerValues(tip.Message, TrailerMerge))
	require.Regexp(t, `^kb/notes/f\.md kept=src dropped=dst-modify strategy=refuse .* reason=chosen$`,
		strings.Join(TrailerValues(tip.Message, TrailerConflict), "\n"))
}

// On the host, what the merge cannot merge is still refused — the whole merge,
// naming only the unmergeable path — and nothing moves.
func TestMergePushed_MergeFacts_UnmergeableStillRefused(t *testing.T) {
	r, _ := hostFork(t, "merge")
	r.write(pmAgent, "notes.txt", "host\n")
	peerTip := r.write(pmPeer, "notes.txt", "peer\n")
	before := r.tip(pmAgent).Hash

	_, err := r.svc.MergePushed(context.Background(), pmPeer, pmAgent, peerTip, "")
	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"notes.txt"}, conflict.Paths, "the fact merged; only the non-fact is refused")
	require.Equal(t, before, r.tip(pmAgent).Hash, "a refusal moves nothing")
}

// Absent setting on the host: refused exactly as before.
func TestMergePushed_NoSetting_Refuses(t *testing.T) {
	r, peerTip := hostFork(t, "")
	_, err := r.svc.MergePushed(context.Background(), pmPeer, pmAgent, peerTip, "")
	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"kb/notes/f.md"}, conflict.Paths)
}

// The rebase fallback (origin's consensus branch rewound) replays each agent
// commit with the fact-level merge: the consensus side is ontoCommit, the
// record goes on the replayed commit's own message — inside its existing
// trailer paragraph, so its Knomit-Trace still reads.
//
// SABOTAGE: drop the StrategyMergeFacts case in replayCommit (S12b; it falls
// to the default agent-wins arm) → the agent's F wholesale, no trailer → red.
func TestReplay_MergeFacts(t *testing.T) {
	r := newCMRepo(t)
	const agent = "agent/replay-abcd1234"
	r.branch(agent, "main")
	seed := r.write(agent, "kb/notes/f.md", cmFact(t, "base", 0.7))
	require.NoError(t, r.svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), seed)))
	require.NoError(t, r.svc.rh.writeAgentBase(agent, seed))
	ctx := WithTrailers(context.Background(), Trailers{Trace: "t-123"})
	_, err := r.svc.Facts().WriteFact(ctx, agent, "kb/notes/f.md", cmFact(t, "agent body", 0.9), "learn: f", "learn")
	require.NoError(t, err)
	onto := r.write("main", "kb/notes/f.md", cmFact(t, "main body", 0.5, "MainAdded"))

	res, err := r.svc.rh.reconcileAgent(context.Background(), agent, "main", StrategyMergeFacts, true)
	require.NoError(t, err)
	require.Equal(t, ModeRebase, res.Mode)
	tip := r.tip(agent)
	require.Equal(t, []plumbing.Hash{onto}, tip.ParentHashes, "a replay writes no merge commit")
	_, out := r.blob(tip, "kb/notes/f.md")
	f := r.parse(out)
	require.Equal(t, "agent body", f.Body)
	require.Equal(t, 0.9, f.Confidence)
	require.Equal(t, []string{"MainAdded"}, f.Entities)
	require.True(t, strings.HasPrefix(tip.Message, "learn: f\n\n"), tip.Message)
	lines := TrailerValues(tip.Message, TrailerMerge)
	require.Len(t, lines, 1, tip.Message)
	require.Contains(t, lines[0], "decided=body,confidence")
	require.Equal(t, "t-123", TrailerValue(tip.Message, TrailerTrace), "the trace trailer still reads")
}

// replayFork is the rebase-fallback shape: the agent and the rewound
// consensus branch (main) both changed F from the watermark, the agent to a
// more confident body. Returns the repo, the agent branch and onto.
func replayFork(t *testing.T) (*cmRepo, string, plumbing.Hash) {
	r := newCMRepo(t)
	const agent = "agent/replay-abcd1234"
	r.branch(agent, "main")
	r.write(agent, "notes.txt", "base\n")
	seed := r.write(agent, "kb/notes/f.md", cmFact(t, "base", 0.7))
	require.NoError(t, r.svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), seed)))
	require.NoError(t, r.svc.rh.writeAgentBase(agent, seed))
	_, err := r.svc.Facts().WriteFact(context.Background(), agent, "kb/notes/f.md", cmFact(t, "agent body", 0.9), "learn: f", "learn")
	require.NoError(t, err)
	r.write("main", "kb/notes/f.md", cmFact(t, "main body", 0.5))
	return r, agent, r.tip("main").Hash
}

// B1 (a): merge:upstream on the replay takes ONTO's version of a field both
// changed — onto is the consensus side here, although it is dst.
//
// SABOTAGE: replayCommit's factMerge with upstream: fact.MergeSrc (S18a) →
// the agent's body → red.
func TestReplay_MergeFactsUpstream_OntoIsUpstream(t *testing.T) {
	r, agent, _ := replayFork(t)
	_, err := r.svc.rh.reconcileAgent(context.Background(), agent, "main", StrategyMergeFactsUpstream, true)
	require.NoError(t, err)
	tip := r.tip(agent)
	_, out := r.blob(tip, "kb/notes/f.md")
	require.Equal(t, "main body", r.parse(out).Body, "upstream = onto (the rewound consensus branch)")
	lines := TrailerValues(tip.Message, TrailerMerge)
	require.Len(t, lines, 1, tip.Message)
	require.Contains(t, lines[0], "strategy=upstream ")
}

// B1 (b): what the replay cannot merge keeps today's replay rule — the
// agent's commit wins — and says so.
//
// SABOTAGE: replayCommit's factMerge with fallback: StrategyLocalWins (S18b)
// → onto's notes.txt, kept=dst → red.
func TestReplay_MergeFacts_FallbackAgentWins(t *testing.T) {
	r, agent, _ := replayFork(t)
	r.write("main", "notes.txt", "main\n")
	r.write(agent, "notes.txt", "agent\n")
	_, err := r.svc.rh.reconcileAgent(context.Background(), agent, "main", StrategyMergeFacts, true)
	require.NoError(t, err)
	tip := r.tip(agent)
	_, notes := r.blob(tip, "notes.txt")
	require.Equal(t, "agent\n", notes, "not a fact: the agent's commit wins, as the replay always did")
	conflicts := strings.Join(TrailerValues(tip.Message, TrailerConflict), "\n")
	require.Regexp(t, `notes\.txt kept=src dropped=dst-modify strategy=remote_wins .* reason=not-a-fact`, conflicts, tip.Message)
}

// N3: with the setting absent, every side-pick the LocalWins walk makes is
// recorded — one line per path, each naming what was dropped.
//
// SABOTAGE: record only the first side-pick → red.
func TestPeerSync_Off_RecordsEverySidePick(t *testing.T) {
	r := newCMRepo(t)
	const consensus, agent = "master", "agent/peer-abcd1234"
	r.write("main", "kb/notes/f.md", cmFact(t, "base", 0.7))
	r.write("main", "kb/notes/d.md", cmFact(t, "base", 0.7))
	r.branch(consensus, "main")
	r.branch(agent, "main")
	r.write(consensus, "kb/notes/f.md", cmFact(t, "consensus", 0.9))
	r.write(agent, "kb/notes/f.md", cmFact(t, "agent", 0.5))
	r.del(consensus, "kb/notes/d.md")
	r.write(agent, "kb/notes/d.md", cmFact(t, "agent edit", 0.7))
	r.write(consensus, "x.txt", "consensus\n")
	r.write(agent, "x.txt", "agent\n")
	r.setOriginTip(consensus)

	_, err := r.svc.Remote().(*remoteIndex).reconcileNow(context.Background(), agent, consensus)
	require.NoError(t, err)
	tip := r.tip(agent)
	lines := TrailerValues(tip.Message, TrailerConflict)
	require.Len(t, lines, 3, tip.Message)
	require.Regexp(t, `^kb/notes/d\.md kept=dst dropped=src-delete strategy=local_wins base=[0-9a-f]{40} src=none dst=[0-9a-f]{40}$`, lines[0])
	require.Regexp(t, `^kb/notes/f\.md kept=dst dropped=src-modify strategy=local_wins `, lines[1])
	require.Regexp(t, `^x\.txt kept=src dropped=dst-add strategy=local_wins base=none `, lines[2])
	require.Empty(t, TrailerValues(tip.Message, TrailerMerge))
}

// The trailer paragraph: appended to an existing trailer block, else a new
// last paragraph; sorted by path.
func TestAppendTrailerLines(t *testing.T) {
	require.Equal(t, "merge: a into b (x)\n\nKnomit-Conflict: a.md k\nKnomit-Merge: b.md m\n",
		appendTrailerLines("merge: a into b (x)", []string{"Knomit-Merge: b.md m", "Knomit-Conflict: a.md k"}))
	require.Equal(t, "learn: x\n\nKnomit-Trace: t\nKnomit-Merge: f.md m\n",
		appendTrailerLines("learn: x\n\nKnomit-Trace: t\n", []string{"Knomit-Merge: f.md m"}))
	require.Equal(t, "learn: x\n\nbody words here.\n\nKnomit-Merge: f.md m\n",
		appendTrailerLines("learn: x\n\nbody words here.", []string{"Knomit-Merge: f.md m"}))
	require.Equal(t, "same", appendTrailerLines("same", nil))
}
