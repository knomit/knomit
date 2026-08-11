package main

import (
	"fmt"
	"math/rand"
)

// assignSharedRefGroups partitions a subset of slots into small groups (2-4
// members) that each cite the same synthetic external URL, as if independent
// sources reporting on the same underlying event. Targets
// .claude/plans/yake-live-discovery-shared-refs-signal.md's open question:
// shared external refs are a cheap, structurally-detectable signal for
// "these facts are already-stated siblings," a distinct sub-case of the
// dominant bridge-rejection reason found in that research.
func assignSharedRefGroups(slots []factSlot, topic string, rate float64, rng *rand.Rand) {
	targetCount := int(float64(len(slots)) * rate)
	if targetCount < 2 {
		return
	}
	avail := shuffledIndices(len(slots), rng)
	pos := 0
	groupNum := 0
	for pos < len(avail) && pos < targetCount {
		groupSize := 2 + rng.Intn(3) // 2-4
		if pos+groupSize > len(avail) {
			groupSize = len(avail) - pos
		}
		if groupSize < 2 {
			break
		}
		url := fmt.Sprintf("https://example.org/%s/%d", topic, groupNum)
		for _, idx := range avail[pos : pos+groupSize] {
			slots[idx].SharedRefURL = url
		}
		pos += groupSize
		groupNum++
	}
}

// assignKeywordGroups partitions a subset of slots into small groups (3-5
// members — see the DF-gate note below for why 3 is the floor) that get
// instructed to agree on ONE shared descriptive phrase of their own choosing
// and make it the central subject of an early sentence (see
// keywordGroupInstruction in llmgen.go), rather than being handed a
// pre-scripted phrase.
//
// Two prior designs both failed empirically, traced directly in
// internal/synthesize/yake.go rather than guessed:
//   - A fixed generic phrase ("technical debt") landed verbatim in every
//     group member's body, but never ranked in any single document's own
//     top-K candidates — one incidental mid-paragraph mention, competing
//     against other candidates in the same dense text for very few slots.
//   - A specific named entity (a product/protocol/CVE) is structurally
//     excluded regardless of prominence: yakeDedup drops any single-word
//     candidate whenever a longer (yakeMaxN=2) co-occurring phrase scores at
//     least as well, and natural prose almost always supplies one — so
//     "QUIC"/"MCP" never survive no matter how central they are.
//
// What survives on real data (cyberai-kb.db's actual keyword bridges —
// "billion valuation", "active exploitation", "data center") is a two-word
// descriptive phrase that is the central point of an early sentence, not a
// named entity and not a passing mention.
//
// Groups are windowed to batchSize-sized chunks, like assignResearchHintGroups,
// so every member of a group is visible to the same LLM completion call and
// can actually agree on one phrase — a group split across independent calls
// has no way to coordinate.
func assignKeywordGroups(slots []factSlot, batchSize int, rate float64, rng *rand.Rand) {
	if batchSize < 3 {
		return
	}
	groupID := 1 // 0 means "no group" on factSlot, so groups start at 1
	for start := 0; start < len(slots); start += batchSize {
		end := start + batchSize
		if end > len(slots) {
			end = len(slots)
		}
		window := end - start
		// A keyword must appear in at least minDF documents to clear
		// keywordDFGate's floor (see internal/synthesize/bridge_score.go,
		// minDF = max(3, ...)) before it's even considered as a candidate.
		// A 2-member group can never clear that floor no matter how
		// distinctive the term is, so groups start at 3, not 2.
		if window < 3 || rng.Float64() >= rate*float64(batchSize) {
			continue
		}
		groupSize := 3 + rng.Intn(3) // 3-5
		if groupSize > window {
			groupSize = window
		}
		idx := shuffledIndices(window, rng)[:groupSize]
		for _, i := range idx {
			slots[start+i].KeywordGroupID = groupID
		}
		groupID++
	}
}

// researchAngles is a rotating list of generic real-world subject categories
// used for the real-content-mode shared-citation mechanic: a group of facts
// is told to each research the SAME specific real event/story within one of
// these categories (their choice which one, as long as it's genuinely real),
// from different angles. Unlike sharedKeywordPhrases these aren't literal
// phrases to weave in — they're research prompts.
var researchAngles = []string{
	"a specific real cybersecurity vulnerability, breach, or threat-actor campaign disclosed recently",
	"a specific real product launch, funding round, or corporate acquisition",
	"a specific real regulatory, legal, or policy action",
	"a specific real research finding, benchmark result, or published study",
	"a specific real service outage, disruption, or infrastructure incident",
	"a specific real organizational or leadership change at a notable company",
}

// assignResearchHintGroups is the real-content-mode counterpart to
// assignSharedRefGroups: since real citations can't be scripted (we don't
// know in advance what the model will actually find), this instead groups
// 2-4 slots to independently research the SAME real event from different
// angles, which tends to produce organically-overlapping real citations —
// a more realistic version of the shared-refs signal
// (.claude/plans/yake-live-discovery-shared-refs-signal.md) than a scripted
// URL ever could be.
//
// Groups are windowed to batchSize-sized chunks (not shuffled across the
// whole corpus) so a group has a real chance of landing in the same LLM
// completion call — facts assigned the same hint but generated in separate,
// independent calls have no way to actually coordinate on which real story
// they found, defeating the point.
func assignResearchHintGroups(slots []factSlot, batchSize int, rate float64, rng *rand.Rand) {
	if batchSize < 2 {
		return
	}
	angleIdx := 0
	for start := 0; start < len(slots); start += batchSize {
		end := start + batchSize
		if end > len(slots) {
			end = len(slots)
		}
		window := end - start
		if window < 2 || rng.Float64() >= rate*float64(batchSize) {
			continue
		}
		groupSize := 2 + rng.Intn(3) // 2-4
		if groupSize > window {
			groupSize = window
		}
		idx := shuffledIndices(window, rng)[:groupSize]
		hint := researchAngles[angleIdx%len(researchAngles)]
		for _, i := range idx {
			slots[start+i].ResearchHint = hint
		}
		angleIdx++
	}
}

func shuffledIndices(n int, rng *rand.Rand) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	rng.Shuffle(n, func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
	return idx
}
