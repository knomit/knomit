package synthesize

// ReasonChain is the chain shape the graders read: what one engine run
// (E02 §10, `engine.Run` / `knomit_reason`) returns for one question. The
// engine (reason.js on the script host) emits it as JSON; the JSON tags are
// the contract. Fields the graders do not read yet are still part of the
// shape so the engine and the graders agree on one document.
//
// Paths are repo paths as knomit returns them (kb/... or kb://<id>/kb/...);
// the grader maps them back to spec fact ids through the loaded world.
type ReasonChain struct {
	// Question is the question's text or id, as the engine was asked.
	Question string `json:"question"`
	// ReasonRun is the run id every log line of the run carries.
	ReasonRun string `json:"reason_run"`
	// World is where the run read: repo, branch and the tip it started at.
	World ChainWorld `json:"world"`
	// Steps in execution order, backtracked ones included.
	Steps []ReasonStep `json:"steps"`
	// Answers are the emitted answer facts (empty on a gap).
	Answers []AnswerFact `json:"answers,omitempty"`
	// Gap is set when the run declares a gap.
	Gap *ChainGap `json:"gap,omitempty"`
	// Assess is the final ASSESS judgment: answers, partly or not.
	Assess string `json:"assess"`
	// Posterior is the answer's posterior (E02 §5), in [0,1].
	Posterior float64 `json:"posterior"`
}

// ChainWorld is the world a run read from.
type ChainWorld struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

// ReasonStep is one state of the loop: the move executed (if any), the facts
// it touched, the judgment the model made, and the posterior after it.
type ReasonStep struct {
	Index int `json:"index"`
	// State is the engine state: GATHER, WIDEN, SYNTHESIZE, HYPOTHESIZE,
	// SEEK, BACKTRACK, ASSESS, EMIT or GAP.
	State string `json:"state"`
	// Move is one of ScenarioMoves, empty for a judgment-only step.
	Move string `json:"move,omitempty"`
	// From is the fact path a fact-anchored move started from.
	From string `json:"from,omitempty"`
	// Arg is the move's string argument (motif, entity, domain, ontology
	// path, type, origin) or "question" for neighbours of the question.
	Arg string `json:"arg,omitempty"`
	// Facts are the facts the step read or returned, each at its commit.
	Facts []FactAt `json:"facts,omitempty"`
	// Judgment is the model's judgment at this step, if one was made.
	Judgment *StepJudgment `json:"judgment,omitempty"`
	// Posterior is the running posterior after the step.
	Posterior float64 `json:"posterior"`
}

// FactAt is a fact at the commit it was read.
type FactAt struct {
	Path   string `json:"path"`
	Commit string `json:"commit"`
}

// StepJudgment is one judgment from E02 §9's closed set.
type StepJudgment struct {
	// Kind: relevant, choose_move, support, synthesize, hypothesize, seek,
	// assess, backtrack_choice.
	Kind string `json:"kind"`
	// Verdicts maps a fact path to its label (relevant: yes|no; support:
	// supports|contradicts|restates|unrelated).
	Verdicts map[string]string `json:"verdicts,omitempty"`
	// Value is a single-valued judgment: the assess label, the chosen menu
	// index, the claim or the search text.
	Value string `json:"value,omitempty"`
}

// AnswerFact is an emitted fact with every field the asker would see.
type AnswerFact struct {
	// Path is where the fact was written (in the run's experiment), if it was.
	Path       string         `json:"path,omitempty"`
	Topic      string         `json:"topic"`
	Category   string         `json:"category"`
	Title      string         `json:"title"`
	Body       string         `json:"body"`
	Kind       string         `json:"kind,omitempty"`
	Type       string         `json:"type"`
	Origin     string         `json:"origin,omitempty"`
	Confidence float64        `json:"confidence"`
	Sources    int            `json:"sources"`
	Domain     []string       `json:"domain"`
	Entities   []string       `json:"entities"`
	Motifs     []string       `json:"motifs,omitempty"`
	Refs       []string       `json:"refs"`
	Context    map[string]any `json:"context,omitempty"`
}

// ChainGap is a structured gap (E02 §8).
type ChainGap struct {
	MissingPremise string `json:"missing_premise"`
	Settlement     string `json:"settlement"`
	// PartialSupport are fact paths the best partial answer rested on.
	PartialSupport []string `json:"partial_support,omitempty"`
	// Hypothesis is the evidence-free hypothesis fact the gap emits.
	Hypothesis *AnswerFact `json:"hypothesis,omitempty"`
}
