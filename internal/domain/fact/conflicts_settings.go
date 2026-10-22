package fact

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// The keys and values of the root attribute `conflicts` (AttrConflicts), an
// object with two keys:
//
//	conflicts:
//	  facts: merge        # off | merge | merge:consensus | consensus
//	  state: consensus    # off | consensus
const (
	ConflictsFacts = "facts" // what a merge does with a FACT both sides changed
	ConflictsState = "state" // what it does with any other path, and with a fact it cannot merge

	ConflictsOff = "off" // the site's own side-pick: a peer's sync keeps its own, the host refuses
	// ConflictsMerge (facts only): MergeVersions, a field both sides changed
	// going to the more confident version.
	ConflictsMerge = "merge"
	// ConflictsMergeConsensus (facts only): MergeVersions, a field both
	// sides changed going to the consensus side's version.
	ConflictsMergeConsensus = "merge:consensus"
	// ConflictsConsensus (facts and state): no field merge — the consensus
	// side's whole version wins.
	ConflictsConsensus = "consensus"
)

// validConflictsFacts and validConflictsState are the closed value sets of the
// two keys. `merge:consensus` is not a state value: there is nothing to merge
// field by field in a path that is not a fact.
func validConflictsFacts(s string) bool {
	return s == ConflictsOff || s == ConflictsMerge || s == ConflictsMergeConsensus || s == ConflictsConsensus
}

func validConflictsState(s string) bool {
	return s == ConflictsOff || s == ConflictsConsensus
}

// validConflicts is the registry's validator: a mapping whose keys are only
// `facts` and `state`, each with a value of its set. The scalar form
// (`conflicts: merge`) is not a value of this attribute.
func validConflicts(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for k, x := range m {
		s, ok := x.(string)
		if !ok {
			return false
		}
		switch k {
		case ConflictsFacts:
			if !validConflictsFacts(s) {
				return false
			}
		case ConflictsState:
			if !validConflictsState(s) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// ConflictsSettings is what a merge site reads from one ontology file.
//
// Valid=false means the key is present with a value the registry rejects (a
// scalar, an unknown inner key, a value a newer knomit understands). Like
// consensus, an unknown value is SAFE to read as off — off is the
// side-picking every site has always done — so the caller does exactly that
// and warns once. Facts and State are both "off" whenever Valid is false:
// an explicit value, even a bad one, is never the `consensus: auto` default.
type ConflictsSettings struct {
	Facts string // ConflictsOff, ConflictsMerge, ConflictsMergeConsensus or ConflictsConsensus
	State string // ConflictsOff or ConflictsConsensus
	Valid bool
	Raw   any // the value as written when Valid is false, for the warning
}

// On reports whether either key changes what a merge does; false is today's
// side-picking everywhere.
func (c ConflictsSettings) On() bool {
	return c.Facts != ConflictsOff || c.State != ConflictsOff
}

// ConflictsFactsRule is the MergeVersions rule a `facts` value names, and
// false when that value does not field-merge (off, consensus).
func ConflictsFactsRule(facts string) (MergeRule, bool) {
	switch facts {
	case ConflictsMerge:
		return MergeConfidence, true
	case ConflictsMergeConsensus:
		return MergeTakeConsensus, true
	}
	return "", false
}

// ReadConflicts reads ONLY the root attributes block of an ontology file, the
// way ReadConsensus does: an unrelated problem in a topic must not change how
// a merge settles a conflict. Values go through the registry validator, never
// the raw map.
//
// A key that is absent reads "off" — unless the same block says
// `consensus: auto`, where an absent `conflicts`, or an absent key of it,
// reads as `facts: merge` / `state: consensus`: a repo whose host merges every
// pushed branch by itself should not leave each conflict for a human by
// default. An explicit "off" stays off.
//
// An error means the file is not YAML, or its root attributes are not a
// mapping; the caller reads that as off.
func ReadConflicts(data []byte) (ConflictsSettings, error) {
	off := ConflictsSettings{Facts: ConflictsOff, State: ConflictsOff}
	var doc struct {
		Attributes yaml.Node `yaml:"attributes"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return off, fmt.Errorf("read conflicts: %w", err)
	}
	out := off
	out.Valid = true
	if doc.Attributes.Kind == 0 {
		return out, nil
	}
	if doc.Attributes.Kind != yaml.MappingNode {
		return off, errors.New("read conflicts: root attributes is not a mapping")
	}
	var attrs map[string]any
	if err := doc.Attributes.Decode(&attrs); err != nil {
		return off, fmt.Errorf("read conflicts: %w", err)
	}
	if c, ok := attrs[AttrConsensus]; ok && attributeRegistry[AttrConsensus].valid(c) && c == ConsensusAuto {
		out.Facts, out.State = ConflictsMerge, ConflictsConsensus
	}
	v, ok := attrs[AttrConflicts]
	if !ok {
		return out, nil
	}
	if !attributeRegistry[AttrConflicts].valid(v) {
		off.Raw = v
		return off, nil
	}
	m := v.(map[string]any)
	if s, ok := m[ConflictsFacts]; ok {
		out.Facts = s.(string)
	}
	if s, ok := m[ConflictsState]; ok {
		out.State = s.(string)
	}
	return out, nil
}
