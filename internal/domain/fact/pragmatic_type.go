package fact

// Pragmatic leaf types — prescriptive knowledge ("what to do").
const (
	Policy    Type = "policy"
	Heuristic Type = "heuristic"
	// Signal is coordination content — a task, a message, an ack, a
	// membership claim, a lease — that travels as a fact because facts are
	// what this system moves, not because it is knowledge.
	//
	// A signal is CONSUMED, not believed. A reader acts on it and is done, so
	// its confidence is meaningless and must never be weighed as evidence.
	// That is the whole reason it is a type rather than an untyped
	// observation: without one, a task gets weighed, dedup-merged, exported
	// and seeded into synthesis like any other claim about the world.
	//
	// Pragmatic rather than epistemic is what buys the behaviour rather than
	// merely labelling it. Kind.AllowsType is the only switch, and being
	// pragmatic already keeps a signal out of the synthesis pipeline
	// (synthesize.reviewStrategy.AcceptSeed), which is what stops it being
	// rewritten as an epistemic fact on commit.
	//
	// The verb is the TOPIC (tasks, inbox, acks, peers, missions, leader),
	// never a sub-type: there is no message/claim/ack type, and adding one is
	// the mistake this single type exists to prevent. The addressee is the
	// PATH, the sender is the commit author, the thread is refs, and the task
	// identity is an ENTITY — which is what keeps the entity-anchored
	// same-subject stage from ever anchoring on one.
	Signal Type = "signal"
	// Frame is a node of a mission's thinking process (a question, a
	// stance, a branch being explored) that is WORKED WITH, not believed.
	// Like a signal it is pragmatic, so its confidence is meaningless, and
	// being pragmatic already keeps it out of synthesis seeding and review
	// dedup (Kind.AllowsType is the switch; no per-type code is needed).
	//
	// The type switches exactly two things: it is accepted under kind
	// pragmatic (and refused under epistemic), and the same-subject refusal
	// in knomit_learn never offers an EXISTING frame as a collision
	// candidate (internal/edge/mcp/learn_same_subject.go), as for signal.
	//
	// It does NOT switch off dedup or merge: an incoming frame is merged,
	// refused or allowed to settle a hypothesis unless its topic sets
	// `learn_dedup: off` (the mission template's frames/ topic does).
	// It does NOT define a status or a topic: frame is a SHAPE, not a verb,
	// so "the verb is the topic" still holds (an abandoned frame is a
	// retracted one). A frame's parents are the frames it refs, any number of
	// them; materialize orders them (ancestors first), and nothing validates
	// them.
	Frame Type = "frame"
)

// PragmaticTypes is the authoritative set of pragmatic Types.
var PragmaticTypes = map[Type]bool{
	Policy:    true,
	Heuristic: true,
	Signal:    true,
	Frame:     true,
}

// AllPragmaticTypes returns all pragmatic Types in a stable order.
func AllPragmaticTypes() []Type {
	return []Type{Policy, Heuristic, Signal, Frame}
}
