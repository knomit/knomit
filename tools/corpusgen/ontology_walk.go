package main

import (
	"fmt"
	"sort"
	"strings"

	"knomit/internal/fact"
)

// describeOntology renders o as an indented topic tree for an LLM prompt.
func describeOntology(o *fact.Ontology) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ontology %q: %s\n", o.ID, o.Description)
	for _, topic := range sortedTopicKeys(o) {
		node := o.Topics[topic]
		fmt.Fprintf(&b, "- %s: %s\n", topic, node.Description)
		for _, child := range sortedChildKeys(node) {
			fmt.Fprintf(&b, "    - %s/%s: %s\n", topic, child, node.Children[child].Description)
		}
	}
	return b.String()
}

// leafCategories returns the direct child keys of topic (e.g. "modules",
// "flows" under "architecture"), sorted for reproducibility. Returns an
// error if topic is not a real node in o — corpusgen only ever picks
// topics/categories drawn from the parsed ontology, so this is the one
// checkpoint that catches a typo'd --topic flag early.
func leafCategories(o *fact.Ontology, topic string) ([]string, error) {
	node, ok := o.Topics[strings.ToLower(topic)]
	if !ok {
		return nil, fmt.Errorf("topic %q not found in ontology %q (known topics: %v)", topic, o.ID, sortedTopicKeys(o))
	}
	children := sortedChildKeys(node)
	if len(children) == 0 {
		// A topic with no children is its own category (BuildFactPath still
		// needs a non-empty category segment).
		return []string{topic}, nil
	}
	return children, nil
}

// excludedTopics are ontology topics corpusgen never generates into by
// default. "principles" (source-code preset) carries custom validation
// requiring kind=pragmatic, type=policy, and a "designer" entity — it's
// meant to be authored only via the /knomit-principle skill, not by a
// bulk content generator. "meta" (both presets) is self-referential
// knowledge-about-the-knowledge-base content ("Methodology facts — lessons
// learned from hypothesis outcomes" / "Knowledge about the knowledge") —
// there's no real-world web-searchable material to ground it in, so
// --diversity broad's automatic topic pool (buildBroadSlots in diversity.go)
// skips it via sortedTopicKeys. An explicit --topic meta still works for
// --diversity narrow, same as any other topic.
var excludedTopics = map[string]bool{
	"principles": true,
	"meta":       true,
}

func sortedTopicKeys(o *fact.Ontology) []string {
	keys := make([]string, 0, len(o.Topics))
	for k := range o.Topics {
		if excludedTopics[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedChildKeys(node *fact.OntologyNode) []string {
	keys := make([]string, 0, len(node.Children))
	for k := range node.Children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
