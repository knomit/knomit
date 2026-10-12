package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// ScenarioAgentBranch is the agent branch a loaded world is written on.
const ScenarioAgentBranch = "agent/scenario"

// ScenarioLoadOptions configures LoadScenario.
type ScenarioLoadOptions struct {
	// Dir holds the world's store (k.db). Required; a test passes t.TempDir().
	Dir string
	// Tools is knomit's in-process write path: mcp.NewScriptTools(embedder),
	// the SAME learn/update/retract/query/explain handlers the MCP server
	// registers. It is a parameter, not built here, because internal/mcp
	// imports this package.
	Tools repos.ScriptTools
	// Embedder, when set, is given to the store and the instance so the
	// learn-time dedup merge and the same-subject gate run as in production.
	// Pass the SAME embedder Tools was built with. Nil: both gates fail open
	// (knomit's own behaviour without an embedder); ValidateFact, ref
	// resolution and the type-mix rule still run.
	Embedder store.BatchEmbedder
}

// LoadedWorld is a scenario materialised as a real knomit repo.
type LoadedWorld struct {
	Spec   *Scenario
	RI     *repos.RepoInstance
	Svc    *store.Service
	Branch string
	// RepoID is the repo's 12-hex id (kb://<RepoID>/... is a local ref).
	RepoID string
	// Paths maps a spec fact id to the path the server minted.
	Paths map[string]string
	// Revisions maps a fact id to the commit of each revision it got, in
	// order: the learn first, then one per update.
	Revisions map[string][]string
	// RetractedAt maps a retracted fact id to the commit that removed it.
	RetractedAt map[string]string
	// StepCommits is the commit each history step made, by step index.
	StepCommits []string
	// Head is the branch tip after the last step.
	Head string

	tools repos.ScriptTools
}

// Ctx returns ctx bound to the world's agent branch, the way the script host
// binds a tool call.
func (w *LoadedWorld) Ctx(ctx context.Context) context.Context {
	return repos.WithBinding(ctx, repos.NewBindingOfRepo(w.RI, w.Branch))
}

// IDOfPath is the reverse of Paths ("" if the path is not a world fact).
func (w *LoadedWorld) IDOfPath(path string) string {
	p := strings.ToLower(path)
	if c := fact.ClassifyRef(path, w.RepoID); c.Kind == fact.RefLocalFact {
		p = c.Path
	}
	for id, wp := range w.Paths {
		if wp == p {
			return id
		}
	}
	return ""
}

// LoadScenario writes the world's ontology at init and replays its history
// through the knomit_learn / knomit_update / knomit_retract handlers. Any
// refusal is returned as a *ScenarioError naming the step, the fact (when it
// can be told) and the server's own text, and the world is unusable: a spec
// the write path refuses is regenerated, never patched (E04 §3).
func LoadScenario(ctx context.Context, s *Scenario, opt ScenarioLoadOptions) (*LoadedWorld, error) {
	if s.parsed == nil {
		if err := s.Check(); err != nil {
			return nil, err
		}
	}
	if opt.Dir == "" || opt.Tools == nil {
		return nil, fmt.Errorf("scenario %s: LoadScenario needs Dir and Tools", s.ID)
	}
	svc, err := store.Open(filepath.Join(opt.Dir, "k.db"))
	if err != nil {
		return nil, fmt.Errorf("scenario %s: open store: %w", s.ID, err)
	}
	if opt.Embedder != nil {
		svc.SetEmbedder(opt.Embedder)
	}
	init := map[string]string{fact.PrivateRoot + "/ontology.yaml": s.Ontology}
	if err := svc.InitRepo(ctx, init, ScenarioAgentBranch); err != nil {
		_ = svc.Close()
		return nil, fmt.Errorf("scenario %s: init repo: %w", s.ID, err)
	}
	root, err := svc.RootCommit(ctx, ScenarioAgentBranch)
	if err != nil {
		_ = svc.Close()
		return nil, fmt.Errorf("scenario %s: root commit: %w", s.ID, err)
	}
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         s.ID,
		UID:          "scenario-" + s.ID,
		AgentBranch:  ScenarioAgentBranch,
		Svc:          svc,
		Ontology:     s.parsed,
		Embedder:     opt.Embedder,
		OntologyRoot: "kb",
	})
	w := &LoadedWorld{
		Spec: s, RI: ri, Svc: svc, Branch: ScenarioAgentBranch, RepoID: fact.ID12(root),
		Paths: map[string]string{}, Revisions: map[string][]string{}, RetractedAt: map[string]string{},
		tools: opt.Tools,
	}
	bctx := w.Ctx(ctx)
	for si, st := range s.History {
		moment := st.Moment
		if moment == "" {
			moment = fmt.Sprintf("scenario step %d", si)
		}
		where := fmt.Sprintf("history[%d]", si)
		var commit string
		switch {
		case len(st.Learn) > 0:
			commit, err = w.learn(bctx, si, st, moment)
		case st.Update != "":
			commit, err = w.update(bctx, st, moment)
		default:
			commit, err = w.retract(bctx, st.Retract, moment)
		}
		if err != nil {
			if se, ok := err.(*ScenarioError); ok {
				return nil, se
			}
			return nil, &ScenarioError{World: s.ID, Where: where, Reason: err.Error()}
		}
		w.StepCommits = append(w.StepCommits, commit)
		w.Head = commit
	}
	return w, nil
}

// Close releases the world's store.
func (w *LoadedWorld) Close() error { return w.Svc.Close() }

func (w *LoadedWorld) resolveRefs(refs []string) ([]string, error) {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if id, ok := strings.CutPrefix(r, FactRefPrefix); ok {
			p, ok := w.Paths[id]
			if !ok {
				return nil, fmt.Errorf("ref %s: fact not written yet", r)
			}
			out = append(out, p)
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// learnArgs is the knomit_learn fact object for f, with refs resolved.
func (w *LoadedWorld) learnArgs(f *ScenarioFact) (map[string]any, error) {
	refs, err := w.resolveRefs(f.Refs)
	if err != nil {
		return nil, err
	}
	m := map[string]any{"topic": f.Topic, "category": f.Category, "title": f.Title, "body": f.Body}
	set := func(k string, v any, ok bool) {
		if ok {
			m[k] = v
		}
	}
	set("kind", f.Kind, f.Kind != "")
	set("type", f.Type, f.Type != "")
	set("confidence", deref(f.Confidence), f.Confidence != nil)
	set("sources", derefInt(f.Sources), f.Sources != nil)
	set("domain", f.Domain, len(f.Domain) > 0)
	set("entities", f.Entities, len(f.Entities) > 0)
	set("motifs", f.Motifs, len(f.Motifs) > 0)
	set("refs", refs, len(refs) > 0)
	set("origin", f.Origin, f.Origin != "")
	set("expires", f.Expires, f.Expires != "")
	set("context", f.Context, len(f.Context) > 0)
	if len(f.DistinctFrom) > 0 {
		var df []string
		for _, id := range f.DistinctFrom {
			df = append(df, w.Paths[id])
		}
		m["distinct_from"] = df
	}
	return m, nil
}

func (w *LoadedWorld) learn(ctx context.Context, si int, st ScenarioStep, moment string) (string, error) {
	var facts []any
	for _, id := range st.Learn {
		m, err := w.learnArgs(w.Spec.FactByID(id))
		if err != nil {
			return "", &ScenarioError{World: w.Spec.ID, Where: "fact " + id, Reason: err.Error()}
		}
		facts = append(facts, m)
	}
	args := map[string]any{"moment_name": moment, "facts": facts}
	if len(st.Retracts) > 0 {
		var rp []string
		for _, id := range st.Retracts {
			rp = append(rp, w.Paths[id])
		}
		args["retract"] = rp
	}
	out, errText, err := w.call(ctx, "learn", args)
	if err != nil {
		return "", err
	}
	if errText != "" {
		for i, id := range st.Learn {
			errText = strings.ReplaceAll(errText, fmt.Sprintf("fact %d:", i), fmt.Sprintf("fact %d (spec %s):", i, id))
		}
		return "", &ScenarioError{World: w.Spec.ID, Where: w.blame(si, st), Reason: "knomit_learn refused: " + w.annotate(errText)}
	}
	var res struct {
		Commits []struct {
			File string `json:"file"`
			Hash string `json:"hash"`
		} `json:"commits"`
		Commit string   `json:"commit"`
		Notes  []string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return "", fmt.Errorf("knomit_learn result: %w", err)
	}
	if len(res.Commits) != len(st.Learn) {
		return "", fmt.Errorf("knomit_learn wrote %d facts for %d", len(res.Commits), len(st.Learn))
	}
	commit := res.Commit
	for i, id := range st.Learn {
		p := res.Commits[i].File
		if other := w.IDOfPath(p); other != "" {
			return "", &ScenarioError{World: w.Spec.ID, Where: "fact " + id,
				Reason: fmt.Sprintf("knomit_learn's dedup merge folded it into fact %s (%s); a world fact must land at its own path: reword it, or name %s in distinct_from if they are deliberately near-identical", other, p, other)}
		}
		for j := 0; j < i; j++ {
			if res.Commits[j].File == p {
				return "", &ScenarioError{World: w.Spec.ID, Where: "fact " + id,
					Reason: fmt.Sprintf("knomit_learn merged it with fact %s of the same call", st.Learn[j])}
			}
		}
		w.Paths[id] = p
		w.Revisions[id] = append(w.Revisions[id], res.Commits[i].Hash)
		commit = res.Commits[i].Hash
	}
	for _, n := range res.Notes {
		if strings.Contains(n, "merged into existing fact") || strings.Contains(n, "dropped") {
			return "", &ScenarioError{World: w.Spec.ID, Where: fmt.Sprintf("history[%d]", si), Reason: "knomit_learn merged a fact: " + n}
		}
	}
	for _, id := range st.Retracts {
		w.RetractedAt[id] = commit
	}
	// knomit_learn settles a hypothesis an incoming observation matches by
	// citing it and retracting it in the same commit (subsumeHypothesis).
	// The spec must say so (retracts), or the gold would treat a retracted
	// hypothesis as live.
	for id, p := range w.Paths {
		if _, gone := w.RetractedAt[id]; gone {
			continue
		}
		live, err := w.Svc.Search().FactLiveAtCommit(ctx, w.Branch, p, commit)
		if err != nil {
			return "", fmt.Errorf("liveness of fact %s: %w", id, err)
		}
		if !live {
			return "", &ScenarioError{World: w.Spec.ID, Where: fmt.Sprintf("history[%d]", si),
				Reason: fmt.Sprintf("knomit_learn retracted fact %s in this commit (an incoming observation subsumed the hypothesis); list it in this step's retracts if the settlement is intended, or reword so it does not match", id)}
		}
	}
	return commit, nil
}

// annotate rewrites every world path in a server message to "path (fact
// <id>)" and every "fact <n>:" index of a refused learn call is left as the
// server wrote it, so an author can map a refusal back to the spec.
func (w *LoadedWorld) annotate(text string) string {
	for id, p := range w.Paths {
		text = strings.ReplaceAll(text, p, p+" (fact "+id+")")
	}
	return text
}

// blame names the fact of a refused learn call that fails the world's
// ontology rules on its own (the server reports only the first failure of the
// call); the step otherwise.
func (w *LoadedWorld) blame(si int, st ScenarioStep) string {
	o := w.Spec.parsed
	for _, id := range st.Learn {
		f := w.Spec.FactByID(id)
		if err := fact.ValidateFact(o, f.Topic+"/"+f.Category, w.preflightFact(f)); err != nil {
			return fmt.Sprintf("history[%d] fact %s (%v)", si, id, err)
		}
	}
	return fmt.Sprintf("history[%d] facts %v", si, st.Learn)
}

// preflightFact is f as ValidateFact would see it, for naming a refusal only;
// the server's verdict is the authority.
func (w *LoadedWorld) preflightFact(f *ScenarioFact) fact.Fact {
	ff := fact.NewFact(fact.BuildFactPath("kb", f.Topic, f.Category))
	ff.Title, ff.Body = f.Title, f.Body
	ff.Kind, ff.Type = fact.Kind(f.Kind), fact.Type(f.Type)
	if ff.Kind == "" {
		ff.Kind = fact.DefaultKind
	}
	if ff.Type == "" {
		ff.Type = fact.DefaultEpistemicType
	}
	ff.Domain, ff.Entities, ff.Motifs = f.Domain, f.Entities, f.Motifs
	ff.Confidence = 0.7
	if f.Confidence != nil {
		ff.Confidence = *f.Confidence
	}
	ff.Sources = 1
	if f.Sources != nil {
		ff.Sources = *f.Sources
	}
	ff.Refs, _ = w.resolveRefs(f.Refs)
	ff.Origin = fact.Origin(f.Origin)
	ff.Expires = f.Expires
	ff.Context = jsonRoundTrip(f.Context)
	return ff
}

func (w *LoadedWorld) update(ctx context.Context, st ScenarioStep, moment string) (string, error) {
	set := map[string]any{}
	for k, v := range st.Set {
		set[k] = v
	}
	if raw, ok := set["refs"].([]any); ok {
		var rs []string
		for _, r := range raw {
			s, _ := r.(string)
			rs = append(rs, s)
		}
		resolved, err := w.resolveRefs(rs)
		if err != nil {
			return "", err
		}
		set["refs"] = resolved
	}
	out, errText, err := w.call(ctx, "update", map[string]any{"file": w.Paths[st.Update], "moment_name": moment, "updates": set})
	if err != nil {
		return "", err
	}
	if errText != "" {
		return "", &ScenarioError{World: w.Spec.ID, Where: "update of fact " + st.Update, Reason: "knomit_update refused: " + w.annotate(errText)}
	}
	var res struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Commit == "" {
		return "", fmt.Errorf("knomit_update result has no commit: %s", out)
	}
	w.Revisions[st.Update] = append(w.Revisions[st.Update], res.Commit)
	return res.Commit, nil
}

func (w *LoadedWorld) retract(ctx context.Context, id, moment string) (string, error) {
	out, errText, err := w.call(ctx, "retract", map[string]any{"file": w.Paths[id], "moment_name": moment})
	if err != nil {
		return "", err
	}
	if errText != "" {
		return "", &ScenarioError{World: w.Spec.ID, Where: "retract of fact " + id, Reason: "knomit_retract refused: " + errText}
	}
	var res struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Commit == "" {
		return "", fmt.Errorf("knomit_retract result has no commit: %s", out)
	}
	w.RetractedAt[id] = res.Commit
	return res.Commit, nil
}

// call runs one tool with args normalised through JSON, exactly as a script's
// arguments arrive (YAML ints become JSON numbers).
func (w *LoadedWorld) call(ctx context.Context, tool string, args map[string]any) (out, errText string, err error) {
	norm := jsonRoundTrip(args)
	text, isErr, err := w.tools.Call(ctx, tool, norm)
	if err != nil {
		return "", "", fmt.Errorf("knomit_%s: %w", tool, err)
	}
	if isErr {
		return "", text, nil
	}
	return text, "", nil
}

func jsonRoundTrip(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return m
	}
	return out
}

func deref(p *float64) float64 { return *p }
func derefInt(p *int) int      { return *p }

// CheckGold is the mechanical half of E04 §3's gold validation, run on a
// loaded world: every fact a question's answer must rest on or cite is live
// at the head (a retracted fact cannot be gold support), no fact is both
// required and forbidden. (The engine writes only `reason_run` into an
// answer's context, and only where the ontology declares it, so the answer
// topic's context declarations are not a gold property.) What it cannot check without the engine — that each
// distractor is reachable by a legal move, that the gap premise is entailed
// by nothing — stays with the teacher runs and the judge.
func CheckGold(ctx context.Context, w *LoadedWorld) error {
	bad := func(q, format string, a ...any) error {
		return &ScenarioError{World: w.Spec.ID, Where: "question " + q + " gold", Reason: fmt.Sprintf(format, a...)}
	}
	liveAtHead := func(id string) (bool, error) {
		return w.Svc.Search().FactLiveAtCommit(ctx, w.Branch, w.Paths[id], w.Head)
	}
	for _, q := range w.Spec.Questions {
		need := append([]string{}, q.Support...)
		if q.Answer != nil {
			need = append(need, q.Answer.MustCite...)
			need = append(need, q.Answer.ShouldCite...)
		}
		if q.Gap != nil {
			need = append(need, q.Gap.PartialSupport...)
		}
		for _, id := range need {
			live, err := liveAtHead(id)
			if err != nil {
				return err
			}
			if !live {
				return bad(q.ID, "fact %s must be cited but is retracted at the head", id)
			}
		}
		if a := q.Answer; a != nil {
			for _, id := range a.MustCite {
				if slices.Contains(a.MustNotCite, id) || slices.Contains(q.Forbidden, id) {
					return bad(q.ID, "fact %s is both required and forbidden", id)
				}
			}
		}
	}
	return nil
}
