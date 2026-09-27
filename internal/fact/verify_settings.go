package fact

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// VerifySettings is what F09 reads from one ontology file: the repository's
// verify_signatures mode.
//
// Valid=false means the key is present with a value its registry spec rejects:
// the setting is UNKNOWN, and the gate must never read it as off. Mode is only
// meaningful when Valid.
type VerifySettings struct {
	Mode  string // "off", "log" or "enforce"; explicit "off" and absent both read "off"
	Valid bool
}

// ReadVerifySettings reads ONLY the root attributes block of an ontology file.
// It does not parse the rest: an unrelated problem in a topic (a bad
// learn_dedup, say) must not make the verify setting unknown and so refuse an
// innocent commit. Values go through the same registry validators as
// ParseOntology, never the raw map (a bad value is kept in Ontology.Attributes
// on the open path, and must not be read as a setting here).
//
// An error means the file is not YAML, or its root attributes are not a
// mapping: the caller treats the setting as UNKNOWN.
func ReadVerifySettings(data []byte) (VerifySettings, error) {
	var doc struct {
		Attributes yaml.Node `yaml:"attributes"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return VerifySettings{}, fmt.Errorf("read verify settings: %w", err)
	}
	out := VerifySettings{Mode: "off", Valid: true}
	if doc.Attributes.Kind == 0 {
		return out, nil
	}
	if doc.Attributes.Kind != yaml.MappingNode {
		return VerifySettings{}, errors.New("read verify settings: root attributes is not a mapping")
	}
	var attrs map[string]any
	if err := doc.Attributes.Decode(&attrs); err != nil {
		return VerifySettings{}, fmt.Errorf("read verify settings: %w", err)
	}
	if v, ok := attrs[AttrVerifySignatures]; ok {
		if !attributeRegistry[AttrVerifySignatures].valid(v) {
			return VerifySettings{Valid: false}, nil
		}
		out.Mode = v.(string)
	}
	return out, nil
}
