package fact

// Pragmatic leaf types — prescriptive knowledge ("what to do").
const (
	Policy    Type = "policy"
	Heuristic Type = "heuristic"
)

// PragmaticTypes is the authoritative set of pragmatic Types.
var PragmaticTypes = map[Type]bool{
	Policy:    true,
	Heuristic: true,
}

// AllPragmaticTypes returns all pragmatic Types in a stable order.
func AllPragmaticTypes() []Type {
	return []Type{Policy, Heuristic}
}
