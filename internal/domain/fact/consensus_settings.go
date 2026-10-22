package fact

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// The values of the root attribute `consensus` (AttrConsensus).
const (
	ConsensusOff  = "off"
	ConsensusAuto = "auto"
)

// ConsensusSettings is what the consensus merger reads from one ontology
// file.
//
// Valid=false means the key is present with a value the registry rejects
// (say, a value a newer knomit understands). Unlike verify_signatures, an
// unknown consensus value is SAFE to read as off — off only means "leave the
// merge to a human" — so the caller does exactly that and warns once. Mode is
// "off" whenever Valid is false.
type ConsensusSettings struct {
	Mode  string // ConsensusOff or ConsensusAuto; absent and explicit "off" both read "off"
	Valid bool
	Raw   any // the value as written when Valid is false, for the warning
}

// ReadConsensus reads ONLY the root attributes block of an ontology file, the
// way ReadVerifySettings does: an unrelated problem in a topic must not change
// what the consensus merger does. Values go through the registry validator,
// never the raw map.
//
// An error means the file is not YAML, or its root attributes are not a
// mapping; the caller reads that as off.
func ReadConsensus(data []byte) (ConsensusSettings, error) {
	var doc struct {
		Attributes yaml.Node `yaml:"attributes"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return ConsensusSettings{Mode: ConsensusOff}, fmt.Errorf("read consensus: %w", err)
	}
	out := ConsensusSettings{Mode: ConsensusOff, Valid: true}
	if doc.Attributes.Kind == 0 {
		return out, nil
	}
	if doc.Attributes.Kind != yaml.MappingNode {
		return ConsensusSettings{Mode: ConsensusOff}, errors.New("read consensus: root attributes is not a mapping")
	}
	var attrs map[string]any
	if err := doc.Attributes.Decode(&attrs); err != nil {
		return ConsensusSettings{Mode: ConsensusOff}, fmt.Errorf("read consensus: %w", err)
	}
	if v, ok := attrs[AttrConsensus]; ok {
		if !attributeRegistry[AttrConsensus].valid(v) {
			return ConsensusSettings{Mode: ConsensusOff, Valid: false, Raw: v}, nil
		}
		out.Mode = v.(string)
	}
	return out, nil
}
