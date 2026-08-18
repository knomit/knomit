package fact

// SchemaField describes one ontology YAML key for editor completions.
type SchemaField struct {
	Struct string `json:"struct"`
	Field  string `json:"field"`
	Doc    string `json:"doc"`
}

// OntologySchema returns the ontology's YAML keys with a one-line description
// each, for the create wizard's editor completions.
//
// This list is covered by TestOntologySchema_CoversEveryYAMLTag: adding a
// yaml-tagged field to Ontology, OntologyNode or Validation without adding it
// here FAILS the build. Do not maintain a parallel copy in TypeScript — the
// web client fetches this over /api/v1/ontologies/schema precisely so there
// is one source of truth.
func OntologySchema() []SchemaField {
	return []SchemaField{
		{"Ontology", "id", "Stable identifier for this ontology (e.g. general, source-code)"},
		{"Ontology", "name", "Human-readable name"},
		{"Ontology", "description", "What this ontology is for"},
		{"Ontology", "topics", "Map of top-level topic keys to their definitions"},
		{"Ontology", "validations", "Rules applied to every fact, whatever its topic"},

		{"OntologyNode", "description", "What this topic covers"},
		{"OntologyNode", "children", "Map of nested sub-topic keys to their definitions"},
		{"OntologyNode", "validations", "Rules applied to facts filed under this topic"},

		{"Validation", "name", "Short identifier for the rule"},
		{"Validation", "message", "Message shown when the rule rejects a fact"},
		{"Validation", "rule", "The rule expression evaluated on write"},
	}
}
