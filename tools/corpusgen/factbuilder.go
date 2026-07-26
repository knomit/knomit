package main

import (
	"fmt"

	"knomit/internal/fact"
)

// buildFact merges a slot's structural assignment with the LLM's generated
// content into a validated fact.Fact plus its on-disk path, mirroring the
// validate → build-path → serialize sequence internal/mcp/learn.go's
// validateAndBuildFacts uses for real learn calls (see learn.go:167).
func buildFact(o *fact.Ontology, ontologyRoot string, slot factSlot, gen generatedContent) (path string, content string, err error) {
	topicCategory := slot.Topic + "/" + slot.Category
	if o != nil {
		if err := o.ValidatePath(topicCategory); err != nil {
			return "", "", fmt.Errorf("slot %d: %w", slot.Index, err)
		}
	}

	path = fact.BuildFactPath(ontologyRoot, slot.Topic, slot.Category)

	domain := gen.Domain
	if domain == nil {
		domain = []string{}
	}
	entities := gen.Entities
	if entities == nil {
		entities = []string{}
	}
	// Safety net: if a shared-entity/keyword instruction was given, ensure it
	// actually shows up as an entity tag even if the model only wove it into
	// prose — this is what makes the fact findable as a keyword/entity bridge
	// member, not just readable as one.
	if slot.SharedKeyword != "" && !containsString(entities, slot.SharedKeyword) {
		entities = append(entities, slot.SharedKeyword)
	}
	// Real mode: gen.Refs carries the model's own (already HTTP-verified —
	// see verify.go) citation URLs. Synthetic mode: gen.Refs is always empty
	// (the synthetic prompt contract has no refs field), so this reduces to
	// the scripted SharedRefURL exactly as before. The two are mutually
	// exclusive by content-source, so no explicit mode branch is needed.
	refs := append([]string{}, gen.Refs...)
	if slot.SharedRefURL != "" {
		refs = append(refs, slot.SharedRefURL)
	}

	f := fact.NewFact(path)
	f.Title = gen.Title
	f.Body = gen.Body
	f.Kind = slot.Kind
	f.Type = slot.Type
	f.Domain = domain
	f.Confidence = slot.Confidence
	f.Sources = slot.Sources
	f.Entities = entities
	f.Refs = refs

	if o != nil {
		if err := fact.ValidateFact(o, topicCategory, f); err != nil {
			return "", "", fmt.Errorf("slot %d: %w", slot.Index, err)
		}
	}

	content, err = fact.SerializeFact(f)
	if err != nil {
		return "", "", fmt.Errorf("slot %d: serialize: %w", slot.Index, err)
	}
	return path, content, nil
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
