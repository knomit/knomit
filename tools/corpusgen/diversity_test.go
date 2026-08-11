package main

import (
	"math/rand"
	"testing"

	"knomit/internal/fact"
)

func TestBuildBroadSlots_SpansEveryTopicAndMixesBatchWindows(t *testing.T) {
	o, err := fact.OntologyByPreset("default")
	if err != nil {
		t.Fatalf("load default ontology: %v", err)
	}

	const size = 200
	const batchSize = 8
	rng := rand.New(rand.NewSource(1))
	slots, err := buildSlots(o, "broad", "", size, "synthetic", batchSize, 0.05, 0.05, rng)
	if err != nil {
		t.Fatalf("buildSlots: %v", err)
	}
	if len(slots) != size {
		t.Fatalf("got %d slots, want %d", len(slots), size)
	}

	seenTopics := map[string]int{}
	for _, s := range slots {
		if s.Topic == "" {
			t.Fatalf("slot %d has empty Topic", s.Index)
		}
		if excludedTopics[s.Topic] {
			t.Fatalf("slot %d assigned excluded topic %q", s.Index, s.Topic)
		}
		seenTopics[s.Topic]++
	}
	wantTopics := sortedTopicKeys(o)
	if len(seenTopics) != len(wantTopics) {
		t.Fatalf("got %d distinct topics %v, want all %d: %v", len(seenTopics), seenTopics, len(wantTopics), wantTopics)
	}

	// At least one batch-sized window should mix more than one topic — if
	// every window were topic-pure, the shuffle-before-windowing step
	// (buildBroadSlots) would have failed to do its job, and keyword/
	// research-hint groups could never span domains.
	mixedWindowFound := false
	for start := 0; start < size; start += batchSize {
		end := start + batchSize
		if end > size {
			end = size
		}
		windowTopics := map[string]bool{}
		for _, s := range slots[start:end] {
			windowTopics[s.Topic] = true
		}
		if len(windowTopics) > 1 {
			mixedWindowFound = true
			break
		}
	}
	if !mixedWindowFound {
		t.Fatal("no batch window mixed more than one topic — topic assignment is still block-cycled, not shuffled")
	}
}

func TestBuildSlots_BroadDoesNotRequireTopic(t *testing.T) {
	o, err := fact.OntologyByPreset("default")
	if err != nil {
		t.Fatalf("load default ontology: %v", err)
	}
	rng := rand.New(rand.NewSource(1))
	if _, err := buildSlots(o, "broad", "", 20, "synthetic", 8, 0.05, 0.05, rng); err != nil {
		t.Fatalf("buildSlots with --diversity broad and no --topic: %v", err)
	}
}
