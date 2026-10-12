package synthesize

// ReasonChain is the result of one engine run, decoded for grading. The
// source of truth is the ENGINE's result, repos.ReasonResult
// (internal/repos/engine.go, emitted by knomit_reason and engine.Run, filled
// by engine_reason.js): these types mirror it field for field with the same
// JSON tags, so a run's JSON decodes here unchanged. When ReasonResult
// changes, these change with it; the graders conform to the engine, never
// the other way round. (They are mirrored rather than imported because
// internal/repos cannot import this package's consumers and this package
// stays free of the engine's runtime.)
//
// Answer facts are not in the result: the result names the paths the HOST
// wrote (AnswerFacts, in the run's experiment) and the grader reads those
// facts from the world's store (GradeChain).
type ReasonChain struct {
	Run              string       `json:"reason_run"`
	Outcome          string       `json:"outcome"` // answer | gap | interrupted | error
	Question         string       `json:"question"`
	Repo             string       `json:"repo"`
	Branch           string       `json:"branch"`
	Experiment       string       `json:"experiment,omitempty"`
	ExperimentBranch string       `json:"experiment_branch,omitempty"`
	AnswerFacts      []string     `json:"answer_facts"`
	ExistingAnswer   string       `json:"existing_answer,omitempty"`
	Posterior        float64      `json:"posterior"`
	Chain            []ReasonStep `json:"chain"`
	Gaps             []ChainGap   `json:"gaps,omitempty"`
	UnwrittenFacts   []any        `json:"unwritten_facts,omitempty"`
	ModelCalls       int          `json:"model_calls"`
	ElapsedMS        int64        `json:"elapsed_ms"`
	Error            string       `json:"error,omitempty"`
	Next             string       `json:"next,omitempty"`
}

// ReasonStep mirrors repos.ReasonStep: one entry of the chain. Move, From and
// Arg name the move the way GoldMove does. Support is the step's per-fact
// labels keyed by fact path: for judgment "support" in GoldStep.Support's
// vocabulary (supports, contradicts, restates, unrelated); for judgment
// "relevant" yes or no.
type ReasonStep struct {
	Step      int               `json:"step"`
	State     string            `json:"state"`
	Move      string            `json:"move,omitempty"`
	From      string            `json:"from,omitempty"`
	Arg       string            `json:"arg,omitempty"`
	Facts     []string          `json:"facts,omitempty"`
	Judgment  string            `json:"judgment,omitempty"`
	Verdict   string            `json:"verdict,omitempty"`
	Support   map[string]string `json:"support,omitempty"`
	Posterior *float64          `json:"posterior,omitempty"`
}

// ChainGap mirrors repos.ReasonGap: a structured gap (E02 §8), returned to the
// asker, not written. The evidence-free hypothesis it comes with is an answer
// fact of type hypothesis.
type ChainGap struct {
	Question       string   `json:"question"`
	Missing        string   `json:"missing,omitempty"`
	BestPartial    string   `json:"best_partial,omitempty"`
	Posterior      float64  `json:"posterior"`
	Tried          []string `json:"tried,omitempty"`
	Contradictions []string `json:"contradictions,omitempty"`
	Reason         string   `json:"reason,omitempty"`
}
