package repos

// Conflict merge PR 1 over real transport: a host instance serving /git and a
// peer instance that clones it, syncs and pushes (the F08 harness). The
// setting is the root attribute `conflicts`, read at the consensus branch's
// tip on both instances; nothing here names that branch.

import (
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

const sharedPath = "kb/tasks/shared.md"

// cmBody is a fact version with a chosen body, confidence and entities.
func cmBody(t *testing.T, body string, conf float64, entities ...string) string {
	t.Helper()
	f := fact.NewFact(sharedPath)
	f.Title, f.Body = "Shared task", body
	f.Kind, f.Type = fact.Epistemic, fact.Observation
	f.Confidence, f.Sources = conf, 1
	f.Domain, f.Refs = []string{"tasks"}, []string{}
	f.Entities = append([]string{}, entities...)
	s, err := fact.SerializeFact(f)
	require.NoError(t, err)
	return s
}

func cmWrite(t *testing.T, ri *RepoInstance, branch, content string) {
	t.Helper()
	_, err := testService(t, ri).Facts().WriteFact(context.Background(), branch, sharedPath, content, "edit", "update")
	require.NoError(t, err)
}

func cmDelete(t *testing.T, ri *RepoInstance, branch string) {
	t.Helper()
	_, err := testService(t, ri).Facts().DeleteFact(context.Background(), branch, sharedPath, "retract")
	require.NoError(t, err)
}

func headCommit(t *testing.T, ri *RepoInstance, branch string) store.CommitInfo {
	t.Helper()
	svc := testService(t, ri)
	h, err := svc.Branches().HeadCommit(context.Background(), branch)
	require.NoError(t, err)
	c, err := svc.Triggers().CommitInfo(context.Background(), plumbing.NewHash(h))
	require.NoError(t, err)
	return c
}

// hasAt reports whether path exists at commit in ri's store.
func hasAt(t *testing.T, ri *RepoInstance, branch string, commit plumbing.Hash, path string) bool {
	t.Helper()
	_, err := testService(t, ri).Facts().ReadFact(context.Background(), branch, path, &store.ReadFactOpts{AtCommit: commit.String()})
	return err == nil
}

func parseShared(t *testing.T, content string) fact.Fact {
	t.Helper()
	f, err := fact.ParseFact(sharedPath, content)
	require.NoError(t, err)
	return f
}

// T6: the peer syncs FIRST (probe shape A with `conflicts: merge`, consensus
// off so only a human merges). The peer's own sync merges the two versions of
// the fact and records it; the host's later merge is clean; the host's agent
// branch and consensus branch hold the peer's merged bytes.
//
// SABOTAGE: keep StrategyLocalWins at the peer's reconcile call site (S12a) →
// the peer keeps its own F, no Knomit-Merge → red.
func TestConflictMerge_PeerSyncsFirst(t *testing.T) {
	h := newConsensusHost(t, triggerOntology("attributes:\n  conflicts:\n    facts: merge\n"), hostOpts{})
	cmWrite(t, h.ri, cHostAgent, cmBody(t, "base", 0.7))
	h.advance(t)
	p := newConsensusPeer(t, h.url)

	cmWrite(t, p.ri, cPeerAgent, cmBody(t, "peer body", 0.9))
	cmWrite(t, h.ri, cHostAgent, cmBody(t, "host body", 0.6, "HostAdded"))
	h.advance(t)

	p.round(t)
	peerTip := headCommit(t, p.ri, cPeerAgent)
	require.Len(t, peerTip.Parents, 2, "the peer's sync wrote a merge commit")
	merges := store.TrailerValues(peerTip.Message, store.TrailerMerge)
	require.Len(t, merges, 1, peerTip.Message)
	require.True(t, strings.HasPrefix(merges[0], sharedPath+" strategy=merge "), merges[0])
	merged := parseShared(t, peerContent(t, p))
	require.Equal(t, "peer body", merged.Body)
	require.Equal(t, []string{"HostAdded"}, merged.Entities)

	res, err := h.svc(t).MergePushed(context.Background(), cPeerAgent, cHostAgent, peerTip.Hash, "")
	require.NoError(t, err, "the host's merge is clean: the base moved to its own tip")
	require.Equal(t, store.ModeMerge, res.Mode)
	h.advance(t)
	require.Equal(t, peerContent(t, p), h.content(t, cHostAgent, sharedPath))
	require.Equal(t, peerContent(t, p), h.content(t, h.upstream(t), sharedPath))
}

func peerContent(t *testing.T, p *cPeer) string {
	t.Helper()
	f, err := testService(t, p.ri).Facts().ReadFact(context.Background(), cPeerAgent, sharedPath, nil)
	require.NoError(t, err)
	return f.Content
}

// T7 + T11: the HOST merges first (`consensus: auto` + `conflicts: merge`).
// The consensus merger merges the fact instead of refusing it — no refusal
// memo — with the record on its merge commit; the peer's next sync converges
// to the host's bytes; three idle rounds add no commit anywhere.
//
// SABOTAGE: MergeConsensus ignoring the attribute (S12d) → refused, memo,
// Refused=1 → red; an asymmetric winner rule (S5) → the peer computes other
// bytes or keeps ping-ponging → red.
func TestConflictMerge_HostMergesFirst_Converges(t *testing.T) {
	h := newConsensusHost(t, triggerOntology("attributes:\n  consensus: auto\n  conflicts:\n    facts: merge\n"), hostOpts{})
	cmWrite(t, h.ri, cHostAgent, cmBody(t, "base", 0.7))
	h.advance(t)
	p := newConsensusPeer(t, h.url)

	cmWrite(t, p.ri, cPeerAgent, cmBody(t, "peer body", 0.9))
	cmWrite(t, h.ri, cHostAgent, cmBody(t, "host body", 0.6, "HostAdded"))
	p.round(t) // the consensus branch has not moved: the peer pushes its edit as is
	h.settle(t)

	s := h.stats()
	require.Zero(t, s.Refused, "merged, not refused: %v", s.Warnings)
	require.Equal(t, 1, s.Merges)
	mc := headCommit(t, h.ri, cHostAgent)
	require.Contains(t, mc.Message, "(conflicts:merge/consensus)", "facts set; state absent under auto → consensus")
	require.Len(t, store.TrailerValues(mc.Message, store.TrailerMerge), 1, mc.Message)
	hostF := h.content(t, cHostAgent, sharedPath)
	merged := parseShared(t, hostF)
	require.Equal(t, "peer body", merged.Body)
	require.Equal(t, 0.9, merged.Confidence)
	require.Equal(t, []string{"HostAdded"}, merged.Entities)

	h.advance(t)
	p.round(t)
	require.Equal(t, hostF, peerContent(t, p), "the peer converges to the host's bytes")

	h.settle(t)
	h.advance(t)
	before := h.commits(t)
	for i := 0; i < 3; i++ {
		p.round(t)
		h.settle(t)
		h.advance(t)
	}
	require.Equal(t, len(before), len(h.commits(t)), "three idle rounds add no commit")
	require.Equal(t, hostF, h.content(t, cHostAgent, sharedPath))
	require.Equal(t, hostF, peerContent(t, p))
}

// T8: modify/delete — the retraction wins, whichever side made it, on both
// instances; the record names the dropped edit and the edit is reachable in
// the merge commit's other parent.
//
// SABOTAGE: edit-wins (resurrect) in factMergeResolutions → the fact is back
// → red.
func TestConflictMerge_RetractionWins(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"host retracts", "peer retracts"} {
		t.Run(shape, func(t *testing.T) {
			h := newConsensusHost(t, triggerOntology("attributes:\n  consensus: auto\n  conflicts:\n    facts: merge\n"), hostOpts{})
			cmWrite(t, h.ri, cHostAgent, cmBody(t, "base", 0.7))
			h.advance(t)
			p := newConsensusPeer(t, h.url)

			wantDropped := "dropped=src-modify" // src = the peer's edit
			if shape == "host retracts" {
				cmWrite(t, p.ri, cPeerAgent, cmBody(t, "peer edit", 0.9))
				cmDelete(t, h.ri, cHostAgent)
			} else {
				cmDelete(t, p.ri, cPeerAgent)
				cmWrite(t, h.ri, cHostAgent, cmBody(t, "host edit", 0.9))
				wantDropped = "dropped=dst-modify"
			}
			p.round(t)
			peerTip := headCommit(t, p.ri, cPeerAgent)
			h.settle(t)
			require.Zero(t, h.stats().Refused)

			mc := headCommit(t, h.ri, cHostAgent)
			lines := store.TrailerValues(mc.Message, store.TrailerMerge)
			require.Len(t, lines, 1, mc.Message)
			require.Contains(t, lines[0], "out=none "+wantDropped+" decided=delete")
			require.False(t, h.has(t, cHostAgent, sharedPath), "the retraction wins on the host")
			h.advance(t)
			require.False(t, h.has(t, h.upstream(t), sharedPath), "and on the consensus branch")

			// The edit is one parent away: the merge commit's second parent
			// (the peer's tip) when the peer edited, its first (the host's
			// previous tip) when the host did.
			require.Equal(t, peerTip.Hash, mc.Parents[1])
			edited := mc.Parents[1]
			if shape == "peer retracts" {
				edited = mc.Parents[0]
			}
			require.True(t, hasAt(t, h.ri, cHostAgent, edited, sharedPath), "the dropped edit stays readable in its parent")

			p.round(t)
			_, err := testService(t, p.ri).Facts().ReadFact(context.Background(), cPeerAgent, sharedPath, nil)
			require.Error(t, err, "the peer converges to the retraction")
		})
	}
}

// T9: with `conflicts` explicitly off everything behaves as before rev 2
// (probe shape A: the host refuses; the peer's LocalWins sync keeps its own
// version; the retry is clean and the peer's version lands) — and the peer's
// merge commit now RECORDS the host edit it dropped. Under `consensus: auto`
// an ABSENT `conflicts` no longer means this (it defaults to merge/consensus,
// PR 2); an explicit off does.
//
// SABOTAGE: an explicit off read as the auto default → the host merges
// instead of refusing → red; the walk's Knomit-Conflict line dropped (S10b)
// → red.
func TestConflictMerge_ExplicitOff_IsToday_Recorded(t *testing.T) {
	h := newConsensusHost(t, triggerOntology(autoOffAttrs), hostOpts{})
	cmWrite(t, h.ri, cHostAgent, cmBody(t, "base", 0.7))
	h.advance(t)
	p := newConsensusPeer(t, h.url)

	cmWrite(t, p.ri, cPeerAgent, cmBody(t, "peer body", 0.5))
	cmWrite(t, h.ri, cHostAgent, cmBody(t, "host body", 0.9))
	p.round(t)
	h.settle(t)
	require.Equal(t, 1, h.stats().Refused, "off: the host refuses as before")

	writeOn(t, h.ri, cHostAgent, "kb/tasks/other.md")
	h.advance(t)
	p.round(t)
	peerTip := headCommit(t, p.ri, cPeerAgent)
	require.Contains(t, peerTip.Message, "(local_wins)")
	require.Empty(t, store.TrailerValues(peerTip.Message, store.TrailerMerge))
	conflicts := store.TrailerValues(peerTip.Message, store.TrailerConflict)
	require.Len(t, conflicts, 1, peerTip.Message)
	require.True(t, strings.HasPrefix(conflicts[0], sharedPath+" kept=dst dropped=src-modify strategy=local_wins "), conflicts[0])

	h.settle(t)
	require.Equal(t, 1, h.stats().Merges)
	require.Equal(t, "peer body", parseShared(t, h.content(t, cHostAgent, sharedPath)).Body,
		"F1 unchanged when the setting is off: the peer's version lands")
}
