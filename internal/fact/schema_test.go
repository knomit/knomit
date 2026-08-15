package fact

import (
	"reflect"
	"strings"
	"testing"
)

// Every yaml-tagged field on the three ontology structs MUST appear in
// OntologySchema. A completion list that has drifted from the Go structs
// teaches confidently wrong field names, which is worse than no completions.
func TestOntologySchema_CoversEveryYAMLTag(t *testing.T) {
	types := map[string]reflect.Type{
		"Ontology":     reflect.TypeOf(Ontology{}),
		"OntologyNode": reflect.TypeOf(OntologyNode{}),
		"Validation":   reflect.TypeOf(Validation{}),
	}
	have := map[string]bool{}
	for _, f := range OntologySchema() {
		have[f.Struct+"."+f.Field] = true
	}
	for structName, typ := range types {
		for i := 0; i < typ.NumField(); i++ {
			sf := typ.Field(i)
			if sf.PkgPath != "" {
				continue // unexported (e.g. Ontology.cache)
			}
			tag := strings.Split(sf.Tag.Get("yaml"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			if !have[structName+"."+tag] {
				t.Errorf("OntologySchema missing %s.%s — add it", structName, tag)
			}
		}
	}
}

func TestOntologySchema_EveryEntryHasDoc(t *testing.T) {
	for _, f := range OntologySchema() {
		if strings.TrimSpace(f.Doc) == "" {
			t.Errorf("%s.%s has no doc — it is shown in the editor completion popup", f.Struct, f.Field)
		}
	}
}
