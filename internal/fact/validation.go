package fact

import (
	"fmt"

	"github.com/dop251/goja"
)

// compiledRule is a Validation with its rule precompiled to a goja.Program.
type compiledRule struct {
	Name    string
	Message string
	Program *goja.Program
}

// compileRules compiles every rule under a topic path. The topic argument is
// used only in error messages so an ontology load failure points at the
// right place.
func compileRules(topic string, rules []Validation) ([]compiledRule, error) {
	out := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		prog, err := goja.Compile(fmt.Sprintf("%s/%s", topic, r.Name), r.Rule, true)
		if err != nil {
			return nil, fmt.Errorf("compile rule %q at %s: %w", r.Name, topic, err)
		}
		out = append(out, compiledRule{Name: r.Name, Message: r.Message, Program: prog})
	}
	return out, nil
}
