package main

import (
	"fmt"
	"math/rand"
)

// sharedKeywordPhrases is a small rotating list of generic, cross-cutting
// technical concepts a batch can be instructed to weave into otherwise
// unrelated facts. This is what manufactures genuine keyword-anchored bridge
// candidates for calibrate bridges to find: the whole point of the "keyword"
// bridge kind is a token shared across topically-distant facts, and organic
// generation alone gives no guarantee any such overlap will occur.
var sharedKeywordPhrases = []string{
	"technical debt",
	"backwards compatibility",
	"race condition",
	"circular dependency",
	"cache invalidation",
	"idempotency",
	"observability",
	"blast radius",
	"single point of failure",
	"eventual consistency",
}

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

// assignKeywordGroups partitions a subset of slots into small groups (2-5
// members) that each get instructed to organically mention the same
// cross-cutting phrase from sharedKeywordPhrases, cycling through the list.
func assignKeywordGroups(slots []factSlot, rate float64, rng *rand.Rand) {
	targetCount := int(float64(len(slots)) * rate)
	if targetCount < 2 {
		return
	}
	avail := shuffledIndices(len(slots), rng)
	pos := 0
	phraseIdx := 0
	for pos < len(avail) && pos < targetCount {
		groupSize := 2 + rng.Intn(4) // 2-5
		if pos+groupSize > len(avail) {
			groupSize = len(avail) - pos
		}
		if groupSize < 2 {
			break
		}
		phrase := sharedKeywordPhrases[phraseIdx%len(sharedKeywordPhrases)]
		for _, idx := range avail[pos : pos+groupSize] {
			slots[idx].SharedKeyword = phrase
		}
		pos += groupSize
		phraseIdx++
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
