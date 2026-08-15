package web

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/fact"
	"knomit/internal/web/hal"
)

// MaxOntologyBytes caps an ontology document accepted by :validate (whose
// whole body is the YAML) and by create (whose body carries it as
// ontology_yaml, and is capped as a whole — the rest of that envelope is a
// few hundred bytes). Both enforce it with http.MaxBytesReader and answer 413;
// neither path may read an ontology without it. Sits alongside
// MaxRepoDescriptionBytes as a request-size guard.
const MaxOntologyBytes = 256 * 1024

// presetNames is the ordered preset list. "default" leads because it is the
// wizard's initial selection.
var presetNames = []string{"default", "code"}

type ontologyValidateResponse struct {
	OK          bool              `json:"ok"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Topics      []string          `json:"topics"`
	RuleCount   int               `json:"rule_count"`
	Diagnostics []fact.Diagnostic `json:"diagnostics,omitempty"`
}

// handleOntologyValidate serves POST /api/v1/ontologies:validate. The request
// body is raw YAML.
//
// An ontology that does not parse is a 200 carrying ok:false, NOT a 4xx: the
// client renders the diagnostics inline, and an HTTP error would conflate "you
// typed a bad key" with "your request was malformed".
func handleOntologyValidate() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxOntologyBytes))
		if err != nil {
			hal.WriteProblem(w, http.StatusRequestEntityTooLarge, "Ontology too large",
				"ontology exceeds the maximum accepted size", r.URL.Path)
			return
		}
		o, diags := fact.ValidateOntologyYAML(body)
		w.Header().Set("Content-Type", "application/json")
		if len(diags) > 0 {
			// Topics has no omitempty (see ontologyValidateResponse), so it must be
			// a non-nil empty slice here — a zero-value nil would encode as JSON
			// null, and the TypeScript client declares this field string[].
			_ = json.NewEncoder(w).Encode(ontologyValidateResponse{OK: false, Topics: []string{}, Diagnostics: diags})
			return
		}
		_ = json.NewEncoder(w).Encode(ontologyValidateResponse{
			OK:        true,
			ID:        o.ID,
			Name:      o.Name,
			Topics:    o.TopicNames(),
			RuleCount: countRules(o),
		})
	}
}

// countRules totals the validations declared at the root and on every node.
func countRules(o *fact.Ontology) int {
	n := len(o.Validations)
	var walk func(*fact.OntologyNode)
	walk = func(node *fact.OntologyNode) {
		if node == nil {
			return
		}
		n += len(node.Validations)
		for _, c := range node.Children {
			walk(c)
		}
	}
	for _, t := range o.Topics {
		walk(t)
	}
	return n
}

type presetSummary struct {
	Name        string   `json:"name"`
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Topics      []string `json:"topics"`
}

// handleOntologyPresets serves GET /api/v1/ontologies/presets.
func handleOntologyPresets() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := make([]presetSummary, 0, len(presetNames))
		for _, name := range presetNames {
			o, err := fact.OntologyByPreset(name)
			if err != nil {
				continue
			}
			out = append(out, presetSummary{
				Name: name, ID: o.ID, Title: o.Name,
				Description: o.Description, Topics: o.TopicNames(),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"presets": out})
	}
}

// handleOntologyPresetYAML serves GET /api/v1/ontologies/presets/{name}. The
// wizard seeds its editor from this, so it returns the same serialized form
// that create would commit.
func handleOntologyPresetYAML() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		o, err := fact.OntologyByPreset(name)
		if err != nil {
			hal.WriteProblem(w, http.StatusNotFound, "Unknown preset", err.Error(), r.URL.Path)
			return
		}
		y, err := o.Serialize()
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Serialize failed", err.Error(), r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		_, _ = w.Write(y)
	}
}

// handleOntologySchema serves GET /api/v1/ontologies/schema — the field list
// backing the editor's completions. Served from Go so there is exactly one
// description of the ontology shape; see TestOntologySchema_CoversEveryYAMLTag.
func handleOntologySchema() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"fields": fact.OntologySchema()})
	}
}
