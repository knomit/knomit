package synthesize

// Scenario worlds (expert spike, E04): a small, real knomit KB built from a
// spec, plus questions whose gold reasoning chain is known by construction.
//
// FORMAT knomit-scenario/v1 — one YAML file per world, `spec.yaml`, decoded
// STRICTLY (an unknown key anywhere is an error, so a typo never silently
// drops a field). The frozen field reference is the doc comment on each type
// below; scenario_example_test.go loads a complete example world.
//
//	format: knomit-scenario/v1        # required, exact
//	id: kebab-case-world-id           # required, unique across all worlds
//	families: [F6, F7, F11]           # family ids (E04 §2 and the scenarios repo); first is primary
//	ontology_shape: code              # free label: code | general | <generated name>
//	description: one paragraph, for humans; never loaded into the world
//	ontology: |                       # the world's own .knomit/ontology.yaml, verbatim
//	  id: ...
//	ontology_file: ../../x.yaml       # OR a path (relative to the spec file) to that YAML;
//	                                  # exactly one of ontology / ontology_file
//	external_sources:                 # stub https:// sources facts may cite; never fetched
//	  - https://example.invalid/...
//	facts: [ScenarioFact...]          # every fact, each written by exactly one learn step
//	history: [ScenarioStep...]        # the commit sequence, in order
//	questions: [ScenarioQuestion...]  # 1-3 per world, each with gold
//
// The loader (scenario_load.go) writes the ontology at init on the consensus
// branch and then replays `history` through knomit's OWN write path (the
// knomit_learn / knomit_update / knomit_retract handlers, in process), so
// every fact passes ValidateFact, ref resolution, dedup and the same-subject
// gate, and has a real git history knomit_explain walks.
//
// Spec-local ids, never paths: a fact's path is minted by the server, so
// everything in a spec names facts by their `id`, and a ref to another world
// fact is written `fact:<id>`. The cited fact must be written by an EARLIER
// history step (one learn call cannot cite a path it has not been told yet)
// and must not be retracted at that point.
//
// Authoring rules the write path enforces (a world that breaks one does not
// load; regenerate it):
//
//   - Motifs are 2-4 kebab-case words and must not repeat the fact's subject
//     (entity, domain or path words are stripped as subject motifs). A motif
//     is shared across DIFFERENT subjects: that is what the motif move uses.
//   - One learn step may not mix observed types (observation, concept,
//     process, principle, pattern, reference, synthesis, insight) with
//     inferred ones (hypothesis, methodology).
//   - Dedup merge: a fact near-identical to an existing fact in the same
//     category is FOLDED into it (the loader reports "dedup merge folded it
//     into fact X"). Same-subject gate: a fact similar to an existing fact
//     anywhere that shares a specific entity is REFUSED. The two escapes are
//     knomit's own: cite the other fact in refs (declared lineage), or name it
//     in distinct_from (a deliberate contradiction, a counter-hypothesis, a
//     trial that overturns a synthesis). Both gates need the embedder; the
//     EXPERT_SPEC_DIR test runs with it on.
//   - Every topic a fact's context uses must declare those keys; root-level
//     and topic validations in the ontology are JavaScript rules over `fact`.
//
// What is NEVER written into the world:`role`, `description`, `families`,
// the questions and their gold. Those are the generator's labels; writing
// them would hand the engine the answer (E04 §7, generator tells).

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"knomit/internal/fact"
)

// ScenarioFormat is the only format string a v1 spec may carry.
const ScenarioFormat = "knomit-scenario/v1"

// ScenarioSpecFile is the file name the directory walk loads.
const ScenarioSpecFile = "spec.yaml"

// FactRefPrefix marks a ref to another fact of the same world by spec id.
const FactRefPrefix = "fact:"

// Scenario is one world.
type Scenario struct {
	Format          string             `yaml:"format"`
	ID              string             `yaml:"id"`
	Families        []string           `yaml:"families"`
	OntologyShape   string             `yaml:"ontology_shape"`
	Description     string             `yaml:"description"`
	Ontology        string             `yaml:"ontology"`
	OntologyFile    string             `yaml:"ontology_file"`
	ExternalSources []string           `yaml:"external_sources"`
	Facts           []ScenarioFact     `yaml:"facts"`
	History         []ScenarioStep     `yaml:"history"`
	Questions       []ScenarioQuestion `yaml:"questions"`

	// parsed is Ontology parsed by fact.ParseOntology during Check.
	parsed *fact.Ontology
}

// ScenarioFact is one fact as the generator authored it. Every field except
// id, role and distinct_from is passed to knomit_learn unchanged (refs after
// fact:<id> resolution).
type ScenarioFact struct {
	// ID is spec-local, kebab or short (f01), unique in the world.
	ID       string `yaml:"id"`
	Topic    string `yaml:"topic"`
	Category string `yaml:"category"`
	// Kind is epistemic (default) or pragmatic.
	Kind string `yaml:"kind"`
	// Type is one of the 13 leaves (fact.EpistemicTypes / fact.PragmaticTypes);
	// required for pragmatic facts, defaults to observation for epistemic.
	Type       string   `yaml:"type"`
	Title      string   `yaml:"title"`
	Body       string   `yaml:"body"`
	Confidence *float64 `yaml:"confidence"`
	Sources    *int     `yaml:"sources"`
	Domain     []string `yaml:"domain"`
	Entities   []string `yaml:"entities"`
	Motifs     []string `yaml:"motifs"`
	// Refs: fact:<id>, an https:// URL listed in external_sources, or a
	// .knomit/<path> system file of the world (e.g. .knomit/ontology.yaml).
	// src:// is refused in v1: a world has no source checkout to resolve it in.
	Refs []string `yaml:"refs"`
	// Origin is authored (default), distilled or discovered.
	Origin string `yaml:"origin"`
	// Expires is RFC 3339.
	Expires string `yaml:"expires"`
	// Context is the F22 map; keys must be declared by the world's ontology.
	Context map[string]any `yaml:"context"`
	// DistinctFrom lists fact ids (written earlier) this fact deliberately
	// sits beside: a planted contradiction or a counter-hypothesis. Sent as
	// knomit_learn's distinct_from, never stored.
	DistinctFrom []string `yaml:"distinct_from"`
	// Role is the generator's planted-structure label (ScenarioRoles). Never
	// written into the world.
	Role string `yaml:"role"`
}

// ScenarioRoles are the planted-structure labels a fact may carry.
var ScenarioRoles = map[string]bool{
	"support":       true, // in some question's gold support set
	"distractor":    true, // same entity or same words, different claim
	"contradiction": true, // a live fact that disagrees with a support fact
	"superseded":    true, // retracted or rewritten by later history
	"hypothesis":    true, // an open or settled prediction
	"settling":      true, // the observation that settles a hypothesis
	"methodology":   true, // changes how the chain must weigh evidence
	"policy":        true, // constrains the answer's shape
	"background":    true, // filler that makes the world realistic
}

// ScenarioStep is one commit of the world's history. Exactly one of Learn,
// Update, Retract is set (Learn may carry Retract-with-it as Retracts).
type ScenarioStep struct {
	// Moment is the knomit moment_name label; defaults to "scenario step N".
	Moment string `yaml:"moment"`
	// Learn writes these facts in ONE knomit_learn call (one commit). A call
	// may not mix observed types with inferred ones (hypothesis, methodology):
	// knomit refuses it.
	Learn []string `yaml:"learn"`
	// Retracts, with Learn, retracts these fact ids in the SAME commit
	// (knomit_learn's atomic move): a settlement ("the observation settles the
	// hypothesis") or a replacement ("A because of C" replaces "A because of
	// B" at a new path).
	Retracts []string `yaml:"retracts"`
	// Update rewrites one fact in place (knomit_update): a supersession that
	// keeps the path, so knomit_explain shows both revisions.
	Update string `yaml:"update"`
	// Set holds the fields Update changes: title, body, kind, type,
	// confidence, sources, domain, entities, motifs, refs (the WHOLE list;
	// fact:<id> resolved), expires, context.
	Set map[string]any `yaml:"set"`
	// Retract retracts one fact on its own (knomit_retract).
	Retract string `yaml:"retract"`
}

// ScenarioMoves are E02 §3's legal moves.
var ScenarioMoves = map[string]bool{
	"neighbours": true, "refs_out": true, "refs_in": true, "motif": true,
	"entity": true, "domain": true, "ontology": true, "kind_type": true,
	"cluster": true, "bridge": true, "history": true, "supersession": true,
	"since": true, "origin": true,
}

// factAnchoredMoves take a fact (From); the others take a string Arg.
var factAnchoredMoves = map[string]bool{
	"refs_out": true, "refs_in": true, "history": true, "supersession": true,
}

// Judgment labels (E02 §9) the gold uses.
var (
	RelevantLabels = map[string]bool{"yes": true, "no": true}
	SupportLabels  = map[string]bool{"supports": true, "contradicts": true, "restates": true, "unrelated": true}
	AssessLabels   = map[string]bool{"answers": true, "partly": true, "not": true}
	OutcomeLabels  = map[string]bool{"answer": true, "partly": true, "gap": true}
)

// ScenarioQuestion is one question with its gold.
type ScenarioQuestion struct {
	ID     string `yaml:"id"`
	Text   string `yaml:"text"`
	Family string `yaml:"family"`
	// Outcome is answer, partly or gap (the gold trust label).
	Outcome string `yaml:"outcome"`
	// Support is the gold support set: the facts the answer must rest on.
	Support []string `yaml:"support"`
	// Forbidden facts must never be cited as support: distractors, retracted
	// or replaced versions, contradicted claims, hypotheses.
	Forbidden []string `yaml:"forbidden"`
	// Chain is the gold chain: steps in a partial order (Requires).
	Chain []GoldStep `yaml:"chain"`
	// Assess is the gold ASSESS judgment: answers, partly or not.
	Assess string `yaml:"assess"`
	// Answer is required for outcome answer or partly.
	Answer *GoldAnswer `yaml:"answer"`
	// Gap is required for outcome gap (and allowed for partly).
	Gap *GoldGap `yaml:"gap"`
}

// GoldStep is one step of the gold chain: a move (any of the alternatives
// counts), the facts it should surface, and the judgments expected on them.
type GoldStep struct {
	ID string `yaml:"id"`
	// Moves are acceptable alternatives; a run move matching any counts.
	Moves []GoldMove `yaml:"moves"`
	// Requires lists gold step ids that must be matched before this one.
	Requires []string `yaml:"requires"`
	// Optional steps are not counted against move recall when skipped.
	Optional bool `yaml:"optional"`
	// Surfaces are the fact ids this move should return.
	Surfaces []string `yaml:"surfaces"`
	// Relevant maps fact id -> yes|no.
	Relevant map[string]string `yaml:"relevant"`
	// Support maps fact id -> supports|contradicts|restates|unrelated.
	Support map[string]string `yaml:"support"`
	// Backtrack marks a dead end the chain must abandon (F13).
	Backtrack bool `yaml:"backtrack"`
}

// GoldMove is one acceptable move. Fact-anchored moves (refs_out, refs_in,
// history, supersession) name From (a fact id); neighbours names From (a
// fact id) or Arg "question"; the rest name Arg (the motif, entity, domain,
// ontology path, type or origin).
type GoldMove struct {
	Move string `yaml:"move"`
	From string `yaml:"from"`
	Arg  string `yaml:"arg"`
}

// GoldAnswer is the gold answer fact's expected shape.
type GoldAnswer struct {
	Claim string `yaml:"claim"`
	// Types are acceptable answer types (e.g. [synthesis, insight]).
	Types []string `yaml:"types"`
	// Topic/Category are where the answer belongs; Category may be a prefix.
	Topic    string `yaml:"topic"`
	Category string `yaml:"category"`
	// MustCite are fact ids the answer's refs must include (normally Support).
	MustCite []string `yaml:"must_cite"`
	// ShouldCite are methodology and policy facts that shaped the chain.
	ShouldCite []string `yaml:"should_cite"`
	// MustNotCite are fact ids the refs must never include.
	MustNotCite []string `yaml:"must_not_cite"`
	// Confidence is the acceptable band [lo, hi].
	Confidence []float64 `yaml:"confidence"`
	// Entities the answer must carry (subset).
	Entities []string `yaml:"entities"`
	// Motifs the answer must carry (subset).
	Motifs []string `yaml:"motifs"`
}

// GoldGap is the gold for a gap: the missing premise and how it would settle.
type GoldGap struct {
	MissingPremise string   `yaml:"missing_premise"`
	Settlement     string   `yaml:"settlement"`
	PartialSupport []string `yaml:"partial_support"`
}

// ScenarioError names what in a spec is wrong: the world, the fact or step,
// and the reason (for a write-path refusal, the server's own text, which
// names the ontology rule).
type ScenarioError struct {
	World  string
	Where  string // "fact f03", "history[4]", "question q1"
	Reason string
}

func (e *ScenarioError) Error() string {
	return fmt.Sprintf("scenario %s: %s: %s", e.World, e.Where, e.Reason)
}

// ParseScenario decodes one spec strictly and checks it (Check). An
// ontology_file is resolved against the working directory; ReadScenarioFile
// resolves it against the spec's own directory.
func ParseScenario(data []byte) (*Scenario, error) { return parseScenarioIn(data, ".") }

func parseScenarioIn(data []byte, dir string) (*Scenario, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("scenario: decode: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("scenario %s: more than one YAML document", s.ID)
	}
	if s.OntologyFile != "" {
		if s.Ontology != "" {
			return nil, &ScenarioError{World: s.ID, Where: "ontology", Reason: "set ontology or ontology_file, not both"}
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(s.OntologyFile)))
		if err != nil {
			return nil, &ScenarioError{World: s.ID, Where: "ontology_file", Reason: err.Error()}
		}
		s.Ontology = string(b)
	}
	if err := s.Check(); err != nil {
		return nil, err
	}
	return &s, nil
}

// ReadScenarioFile reads and parses one spec file.
func ReadScenarioFile(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := parseScenarioIn(b, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// FindScenarioSpecs returns every spec.yaml under root, sorted.
func FindScenarioSpecs(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") && p != root {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == ScenarioSpecFile {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// ParsedOntology is the world's ontology (after Check).
func (s *Scenario) ParsedOntology() *fact.Ontology { return s.parsed }

// FactByID returns the spec fact with id, or nil.
func (s *Scenario) FactByID(id string) *ScenarioFact {
	for i := range s.Facts {
		if s.Facts[i].ID == id {
			return &s.Facts[i]
		}
	}
	return nil
}

var scenarioIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// familyRe accepts F1..F99: E04 §2 lists F1-F16, and the expert-scenarios
// repository extends the list (families/README files are the authority).
var familyRe = regexp.MustCompile(`^F[1-9][0-9]?$`)

// Check is the static half of validation: everything decidable without a
// repo. The write path (scenario_load.go) is the other half; a spec that
// passes Check can still be refused there (an ontology rule, dedup, the
// same-subject gate), and that refusal is final: regenerate, never patch.
func (s *Scenario) Check() error {
	bad := func(where, format string, a ...any) error {
		return &ScenarioError{World: s.ID, Where: where, Reason: fmt.Sprintf(format, a...)}
	}
	if s.Format != ScenarioFormat {
		return bad("format", "is %q, want %q", s.Format, ScenarioFormat)
	}
	if !scenarioIDRe.MatchString(s.ID) {
		return bad("id", "%q is not kebab-case", s.ID)
	}
	if len(s.Families) == 0 {
		return bad("families", "at least one family is required")
	}
	for _, f := range s.Families {
		if !familyRe.MatchString(f) {
			return bad("families", "%q is not a family id (F1..F99)", f)
		}
	}
	if strings.TrimSpace(s.Ontology) == "" {
		return bad("ontology", "the world's own ontology YAML is required")
	}
	o, err := fact.ParseOntology([]byte(s.Ontology))
	if err != nil {
		return bad("ontology", "%v", err)
	}
	s.parsed = o
	sources := map[string]bool{}
	for _, u := range s.ExternalSources {
		if !strings.HasPrefix(u, "https://") {
			return bad("external_sources", "%q is not an https:// URL", u)
		}
		sources[u] = true
	}

	// Facts.
	ids := map[string]bool{}
	for i := range s.Facts {
		f := &s.Facts[i]
		where := fmt.Sprintf("fact %s", f.ID)
		if f.ID == "" || strings.ContainsAny(f.ID, " :@/") {
			return bad(fmt.Sprintf("facts[%d]", i), "id %q is empty or contains one of ' :@/'", f.ID)
		}
		if ids[f.ID] {
			return bad(where, "duplicate id")
		}
		ids[f.ID] = true
		if f.Topic == "" || f.Category == "" {
			return bad(where, "topic and category are required")
		}
		if err := o.ValidatePath(f.Topic + "/" + f.Category); err != nil {
			return bad(where, "path %s/%s: %v", f.Topic, f.Category, err)
		}
		if strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Body) == "" {
			return bad(where, "title and body are required")
		}
		kind := fact.Kind(f.Kind)
		if f.Kind == "" {
			kind = fact.DefaultKind
		}
		if kind != fact.Epistemic && kind != fact.Pragmatic {
			return bad(where, "kind %q is not epistemic or pragmatic", f.Kind)
		}
		if f.Type == "" && kind == fact.Pragmatic {
			return bad(where, "a pragmatic fact must declare its type (policy, heuristic, signal)")
		}
		if f.Type != "" && !kind.AllowsType(fact.Type(f.Type)) {
			return bad(where, "type %q is not a %s type", f.Type, kind)
		}
		if f.Confidence != nil && (*f.Confidence < 0 || *f.Confidence > 1) {
			return bad(where, "confidence %v outside [0,1]", *f.Confidence)
		}
		if f.Origin != "" && f.Origin != "authored" && f.Origin != "distilled" && f.Origin != "discovered" {
			return bad(where, "origin %q is not authored, distilled or discovered", f.Origin)
		}
		if f.Expires != "" {
			if _, err := time.Parse(time.RFC3339, f.Expires); err != nil {
				return bad(where, "expires %q is not RFC 3339", f.Expires)
			}
		}
		if f.Role != "" && !ScenarioRoles[f.Role] {
			return bad(where, "role %q is not one of %s", f.Role, keys(ScenarioRoles))
		}
		if err := fact.ValidateMotifs(f.Motifs); err != nil {
			return bad(where, "%v", err)
		}
		for _, r := range f.Refs {
			if err := checkSpecRef(r, sources); err != nil {
				return bad(where, "%v", err)
			}
		}
	}

	// History: every fact learned exactly once; refs and distinct_from point
	// backwards at live facts; updates and retractions touch live facts.
	learnedAt := map[string]int{}
	live := map[string]bool{}
	for si, st := range s.History {
		where := fmt.Sprintf("history[%d]", si)
		n := 0
		if len(st.Learn) > 0 {
			n++
		}
		if st.Update != "" {
			n++
		}
		if st.Retract != "" {
			n++
		}
		if n != 1 {
			return bad(where, "exactly one of learn, update, retract must be set")
		}
		if len(st.Retracts) > 0 && len(st.Learn) == 0 {
			return bad(where, "retracts is only allowed with learn (an atomic move); use retract for a lone retraction")
		}
		if len(st.Set) > 0 && st.Update == "" {
			return bad(where, "set is only allowed with update")
		}
		switch {
		case len(st.Learn) > 0:
			for _, id := range st.Learn {
				f := s.FactByID(id)
				if f == nil {
					return bad(where, "learn names unknown fact %q", id)
				}
				if _, dup := learnedAt[id]; dup {
					return bad(where, "fact %q is learned twice", id)
				}
				for _, r := range f.Refs {
					if dep, ok := strings.CutPrefix(r, FactRefPrefix); ok && !live[dep] {
						return bad("fact "+id, "ref %s must name a fact written by an EARLIER step and still live", r)
					}
				}
				for _, dep := range f.DistinctFrom {
					if !live[dep] {
						return bad("fact "+id, "distinct_from %s must name a fact written by an earlier step and still live", dep)
					}
				}
			}
			for _, id := range st.Retracts {
				if !live[id] {
					return bad(where, "retracts %q, which is not live at this step", id)
				}
			}
			for _, id := range st.Retracts {
				delete(live, id)
			}
			for _, id := range st.Learn {
				learnedAt[id] = si
				live[id] = true
			}
		case st.Update != "":
			if !live[st.Update] {
				return bad(where, "updates %q, which is not live at this step", st.Update)
			}
			if len(st.Set) == 0 {
				return bad(where, "update needs a non-empty set")
			}
			for k, v := range st.Set {
				if !updatableFields[k] {
					return bad(where, "set.%s is not an updatable field (%s)", k, keys(updatableFields))
				}
				if k == "refs" {
					list, ok := v.([]any)
					if !ok {
						return bad(where, "set.refs must be a list")
					}
					for _, r := range list {
						rs, _ := r.(string)
						if err := checkSpecRef(rs, sources); err != nil {
							return bad(where, "%v", err)
						}
						if dep, ok := strings.CutPrefix(rs, FactRefPrefix); ok && (!live[dep] || dep == st.Update) {
							return bad(where, "ref %s must name another live fact", rs)
						}
					}
				}
			}
		case st.Retract != "":
			if !live[st.Retract] {
				return bad(where, "retracts %q, which is not live at this step", st.Retract)
			}
			delete(live, st.Retract)
		}
	}
	for _, f := range s.Facts {
		if _, ok := learnedAt[f.ID]; !ok {
			return bad("fact "+f.ID, "no history step learns it")
		}
	}

	// Questions.
	if len(s.Questions) == 0 {
		return bad("questions", "at least one question is required")
	}
	qids := map[string]bool{}
	for _, q := range s.Questions {
		where := "question " + q.ID
		if q.ID == "" || qids[q.ID] {
			return bad(where, "id empty or duplicate")
		}
		qids[q.ID] = true
		if strings.TrimSpace(q.Text) == "" {
			return bad(where, "text is required")
		}
		if !familyRe.MatchString(q.Family) {
			return bad(where, "family %q is not a family id (F1..F99)", q.Family)
		}
		if !OutcomeLabels[q.Outcome] {
			return bad(where, "outcome %q is not answer, partly or gap", q.Outcome)
		}
		if !AssessLabels[q.Assess] {
			return bad(where, "assess %q is not answers, partly or not", q.Assess)
		}
		for _, id := range append(append([]string{}, q.Support...), q.Forbidden...) {
			if !ids[id] {
				return bad(where, "names unknown fact %q", id)
			}
		}
		for _, id := range q.Support {
			for _, fid := range q.Forbidden {
				if id == fid {
					return bad(where, "fact %q is both support and forbidden", id)
				}
			}
		}
		if len(q.Chain) == 0 {
			return bad(where, "a gold chain is required")
		}
		steps := map[string]bool{}
		for _, g := range q.Chain {
			sw := fmt.Sprintf("%s chain step %s", where, g.ID)
			if g.ID == "" || steps[g.ID] {
				return bad(sw, "id empty or duplicate")
			}
			for _, r := range g.Requires {
				if !steps[r] {
					return bad(sw, "requires %q, which is not an EARLIER step", r)
				}
			}
			steps[g.ID] = true
			if len(g.Moves) == 0 {
				return bad(sw, "at least one acceptable move is required")
			}
			for _, m := range g.Moves {
				if !ScenarioMoves[m.Move] {
					return bad(sw, "move %q is not one of %s", m.Move, keys(ScenarioMoves))
				}
				switch {
				case factAnchoredMoves[m.Move]:
					if !ids[m.From] || m.Arg != "" {
						return bad(sw, "%s needs from: <fact id> and no arg", m.Move)
					}
				case m.Move == "neighbours":
					if (m.From == "") == (m.Arg == "") || (m.Arg != "" && m.Arg != "question") || (m.From != "" && !ids[m.From]) {
						return bad(sw, "neighbours needs from: <fact id> or arg: question")
					}
				default:
					if m.Arg == "" || m.From != "" {
						return bad(sw, "%s needs arg and no from", m.Move)
					}
				}
			}
			for _, id := range g.Surfaces {
				if !ids[id] {
					return bad(sw, "surfaces unknown fact %q", id)
				}
			}
			for id, v := range g.Relevant {
				if !ids[id] || !RelevantLabels[v] {
					return bad(sw, "relevant %s: %s (want a fact id -> yes|no)", id, v)
				}
			}
			for id, v := range g.Support {
				if !ids[id] || !SupportLabels[v] {
					return bad(sw, "support %s: %s (want a fact id -> %s)", id, v, keys(SupportLabels))
				}
			}
		}
		switch q.Outcome {
		case "answer", "partly":
			if q.Answer == nil {
				return bad(where, "outcome %s needs answer", q.Outcome)
			}
		case "gap":
			if q.Gap == nil || strings.TrimSpace(q.Gap.MissingPremise) == "" || strings.TrimSpace(q.Gap.Settlement) == "" {
				return bad(where, "outcome gap needs gap.missing_premise and gap.settlement")
			}
		}
		if a := q.Answer; a != nil {
			if strings.TrimSpace(a.Claim) == "" || len(a.Types) == 0 || a.Topic == "" {
				return bad(where, "answer needs claim, types and topic")
			}
			for _, t := range a.Types {
				if !fact.Epistemic.AllowsType(fact.Type(t)) {
					return bad(where, "answer type %q is not an epistemic type", t)
				}
			}
			if a.Category != "" {
				if err := o.ValidatePath(a.Topic + "/" + a.Category); err != nil {
					return bad(where, "answer path: %v", err)
				}
			}
			for _, id := range append(append(append([]string{}, a.MustCite...), a.ShouldCite...), a.MustNotCite...) {
				if !ids[id] {
					return bad(where, "answer names unknown fact %q", id)
				}
			}
			if len(a.Confidence) != 0 && (len(a.Confidence) != 2 || a.Confidence[0] > a.Confidence[1]) {
				return bad(where, "answer.confidence must be [lo, hi]")
			}
		}
		if g := q.Gap; g != nil {
			for _, id := range g.PartialSupport {
				if !ids[id] {
					return bad(where, "gap names unknown fact %q", id)
				}
			}
		}
	}
	return nil
}

var updatableFields = map[string]bool{
	"title": true, "body": true, "kind": true, "type": true, "confidence": true,
	"sources": true, "domain": true, "entities": true, "motifs": true,
	"refs": true, "expires": true, "context": true,
}

func checkSpecRef(r string, sources map[string]bool) error {
	switch {
	case strings.HasPrefix(r, FactRefPrefix):
		if strings.TrimPrefix(r, FactRefPrefix) == "" {
			return fmt.Errorf("ref %q names no fact", r)
		}
	case strings.HasPrefix(r, "https://"):
		if !sources[r] {
			return fmt.Errorf("ref %q is not listed in external_sources", r)
		}
	case strings.HasPrefix(r, fact.PrivateRoot+"/"):
	default:
		return fmt.Errorf("ref %q: v1 allows fact:<id>, a listed https:// source, or a .knomit/ system file", r)
	}
	return nil
}

func keys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
