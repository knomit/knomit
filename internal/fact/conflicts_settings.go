package fact

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// The values of the root attribute `conflicts` (AttrConflicts).
const (
	ConflictsOff           = "off"
	ConflictsMerge         = "merge"          // MergeVersions with MergeConfidence
	ConflictsMergeUpstream = "merge:upstream" // MergeVersions with MergeUpstream
)

// ConflictsSettings is what a merge site reads from one ontology file.
//
// Valid=false means the key is present with a value the registry rejects
// (say, a value a newer knomit understands). Like consensus, an unknown value
// is SAFE to read as off — off is the side-picking every site has always done
// — so the caller does exactly that and warns once. Mode is "off" whenever
// Valid is false.
type ConflictsSettings struct {
	Mode  string // ConflictsOff, ConflictsMerge or ConflictsMergeUpstream; absent reads "off"
	Valid bool
	Raw   any // the value as written when Valid is false, for the warning
}

// Rule is the MergeVersions rule the setting names, and false when conflicts
// are not merged (off, absent, or a value this build does not know).
func (c ConflictsSettings) Rule() (MergeRule, bool) {
	switch c.Mode {
	case ConflictsMerge:
		return MergeConfidence, true
	case ConflictsMergeUpstream:
		return MergeUpstream, true
	}
	return "", false
}

// ReadConflicts reads ONLY the root attributes block of an ontology file, the
// way ReadConsensus does: an unrelated problem in a topic must not change how
// a merge settles a conflict. Values go through the registry validator, never
// the raw map.
//
// An error means the file is not YAML, or its root attributes are not a
// mapping; the caller reads that as off.
func ReadConflicts(data []byte) (ConflictsSettings, error) {
	var doc struct {
		Attributes yaml.Node `yaml:"attributes"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return ConflictsSettings{Mode: ConflictsOff}, fmt.Errorf("read conflicts: %w", err)
	}
	out := ConflictsSettings{Mode: ConflictsOff, Valid: true}
	if doc.Attributes.Kind == 0 {
		return out, nil
	}
	if doc.Attributes.Kind != yaml.MappingNode {
		return ConflictsSettings{Mode: ConflictsOff}, errors.New("read conflicts: root attributes is not a mapping")
	}
	var attrs map[string]any
	if err := doc.Attributes.Decode(&attrs); err != nil {
		return ConflictsSettings{Mode: ConflictsOff}, fmt.Errorf("read conflicts: %w", err)
	}
	if v, ok := attrs[AttrConflicts]; ok {
		if !attributeRegistry[AttrConflicts].valid(v) {
			return ConflictsSettings{Mode: ConflictsOff, Valid: false, Raw: v}, nil
		}
		out.Mode = v.(string)
	}
	return out, nil
}
