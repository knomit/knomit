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
// here FAILS the build. Trigger keys come from triggerKeys, the same list the
// trigger parser accepts, so the two cannot drift. Do not maintain a parallel copy in TypeScript — the
// web client fetches this over /api/v1/ontologies/schema precisely so there
// is one source of truth.
func OntologySchema() []SchemaField {
	return append([]SchemaField{
		{"Ontology", "id", "Stable identifier for this ontology (e.g. general, source-code)"},
		{"Ontology", "name", "Human-readable name"},
		{"Ontology", "description", "What this ontology is for"},
		{"Ontology", "topics", "Map of top-level topic keys to their definitions"},
		{"Ontology", "validations", "Rules applied to every fact, whatever its topic"},
		{"Ontology", "attributes", "Repository-level settings. verify_signatures: off (default), log or enforce — verify commit signatures of the upstream for this repository. consensus: off (default) or auto — on the instance that hosts this repository with no origin, merge every branch a peer pushes into this instance's agent branch as soon as it lands (a conflict is left for a human; a repo with an origin ignores it). conflicts: off (default), merge or merge:upstream — when both sides of a merge changed the same fact, merge the two versions field by field against their common ancestor (a field both changed goes to the more confident version, or with merge:upstream to the consensus branch's) instead of picking a side; read at the consensus branch's tip; every conflict a merge settles is recorded on its merge commit"},

		{"OntologyNode", "description", "What this topic covers"},
		{"OntologyNode", "children", "Map of nested sub-topic keys to their definitions"},
		{"OntologyNode", "validations", "Rules applied to facts filed under this topic"},
		{"OntologyNode", "attributes", "Store behaviour for facts under this topic and every sub-topic that does not override it. One key today: learn_dedup: off (or on) — skip knomit_learn's auto-merge and same-subject refusal"},

		{"OntologyNode", "triggers", "Rules fired by changes to facts under this topic: name, match, on, if, do. A bad trigger is skipped with a warning, never fatal"},

		{"Validation", "name", "Short identifier for the rule"},
		{"Validation", "message", "Message shown when the rule rejects a fact"},
		{"Validation", "rule", "The rule expression evaluated on write"},
	}, triggerKeys...)
}
