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
)

// PragmaticTypes is the authoritative set of pragmatic Types.
var PragmaticTypes = map[Type]bool{
	Policy:    true,
	Heuristic: true,
	Signal:    true,
}

// AllPragmaticTypes returns all pragmatic Types in a stable order.
func AllPragmaticTypes() []Type {
	return []Type{Policy, Heuristic, Signal}
}
