package fact

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/dop251/goja"
)

// A `do: script` trigger (F07 PR 3) runs `.knomit/triggers/<name>.js` — repo
// content, read from the observed head — in the same sandbox as `if`, with
// the same three frozen globals (fact, agent, change) plus ONE host object,
// `knomit`, whose functions are the only way out of the VM. This file is the
// sandbox half: compile, bind, run. What each host function does (the
// in-process MCP handlers, the trailers, the `.knomit/` refusal, the
// budget's ctx) is the dispatcher's (internal/repos/trigger_script.go).

// ScriptEvalTimeout is the time budget of ONE script fire — a constant beside
// the 100 ms `if` budget, not a config key: a script's host calls cost
// milliseconds to ~100 ms each (a write, a text query), so it needs seconds
// where a condition needs milliseconds, and a script that legitimately needs
// more is a script to redesign. The budget bounds the JavaScript and the
// NUMBER of host calls (goja's interrupt does not interrupt a native call);
// the host ctx carries the same deadline so no single call outlives it by
// more than its own bounded work.
const ScriptEvalTimeout = 5 * time.Second

// TriggerScriptPath is the repo path of a trigger script: the script name
// (kebab-case, validated at compile time) directly under .knomit/triggers/.
func TriggerScriptPath(name string) string {
	return PrivateRoot + "/triggers/" + name + ".js"
}

// CompileScript compiles a script's source once (strict mode). A Program is
// not tied to a runtime and runs in a fresh one per fire; the dispatcher caches
// it by the script's blob hash.
func CompileScript(name, src string) (*goja.Program, error) {
	return goja.Compile("script "+name, src, true)
}

// ScriptHostFunc is one function of the `knomit` object. args are the call's
// arguments as plain Go values (JSON-shaped: map[string]any, []any, float64,
// string, bool, nil); the result is JSON-encoded back into the script, and an
// error is thrown to the script as an `Error` carrying err.Error(). The host
// owns any recovery from a Go panic inside the call: a panic that reaches the
// sandbox escapes RunScript as a Go panic (goja does not convert host panics).
type ScriptHostFunc func(args []any) (any, error)

// ScriptHost is the `knomit` object: name → function. A map of closures, never
// a Go struct — binding a struct would expose every exported method and field.
type ScriptHost map[string]ScriptHostFunc

// RunScript runs one fire of prog in a fresh sandbox: the frozen globals,
// `Date` pinned to now, and `knomit` built from host (frozen, non-writable).
// The JavaScript is interrupted when ctx is done — the caller derives ctx from
// the dispatcher's own ctx with the budget as its deadline, so budget expiry
// AND dispatcher cancellation stop a running script within the interrupt
// latency, and the same ctx bounds every host call. A `*goja.InterruptedError`
// is returned for either; the caller reads ctx.Err() to tell them apart. The
// program's completion value is ignored; a throw is the error.
func RunScript(ctx context.Context, prog *goja.Program, name string, globals map[string]any, host ScriptHost, now time.Time) error {
	_, err := runSandboxed(ctx, prog, "script "+name, globals, host, now)
	return err
}

// runSandboxed is RunScript's body, returning the program's completion value
// (a script ignores it; a recipe's result is it). who names the program in
// errors ("script x", "recipe y").
func runSandboxed(ctx context.Context, prog *goja.Program, who string, globals map[string]any, host ScriptHost, now time.Time) (goja.Value, error) {
	vm := newSandboxVM(now)
	stop := context.AfterFunc(ctx, func() {
		vm.Interrupt(fmt.Sprintf("%s: %v", who, context.Cause(ctx)))
	})
	defer stop()

	parse, freeze, err := sandboxGlobals(vm, who, globals)
	if err != nil {
		return nil, err
	}
	obj := vm.NewObject()
	for _, fname := range slices.Sorted(maps.Keys(host)) {
		f := host[fname]
		if err := obj.Set(fname, hostFunc(vm, parse, fname, f)); err != nil {
			return nil, fmt.Errorf("%s: bind knomit.%s: %w", who, fname, err)
		}
	}
	if _, err := freeze(goja.Undefined(), obj); err != nil {
		return nil, fmt.Errorf("%s: freeze knomit: %w", who, err)
	}
	if err := vm.GlobalObject().DefineDataProperty("knomit", obj, goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_FALSE); err != nil {
		return nil, fmt.Errorf("%s: bind knomit: %w", who, err)
	}
	v, err := vm.RunProgram(prog)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", who, err)
	}
	return v, nil
}

// hostFunc adapts one ScriptHostFunc to the VM: arguments exported as plain
// values, the result parsed back in, an error thrown as a JavaScript Error.
func hostFunc(vm *goja.Runtime, parse goja.Callable, name string, f ScriptHostFunc) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		args := make([]any, len(call.Arguments))
		for i, a := range call.Arguments {
			v, err := plainValue(a)
			if err != nil {
				throwJS(vm, fmt.Errorf("knomit.%s: argument %d: %w", name, i, err))
			}
			args[i] = v
		}
		out, err := f(args)
		if err != nil {
			throwJS(vm, err)
		}
		if out == nil {
			return goja.Undefined()
		}
		v, err := jsValue(vm, parse, out)
		if err != nil {
			throwJS(vm, fmt.Errorf("knomit.%s: result: %w", name, err))
		}
		return v
	}
}

// plainValue exports a JavaScript value as a JSON-shaped Go value: objects to
// map[string]any, arrays to []any, numbers to float64, undefined/null to nil.
// The JSON round trip is what makes an integer written in JavaScript arrive as
// the float64 every JSON-decoded argument already is, so a handler's argument
// decoding sees the same shapes the MCP transport gives it.
func plainValue(v goja.Value) (any, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	b, err := json.Marshal(v.Export())
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// throwJS throws err into the running script as `new Error(message)` — a real
// JavaScript Error (`instanceof Error`, `.message`), never a wrapped Go value.
// goja turns a panic with a Value inside a host function into the script's
// exception; the panic never leaves the VM.
func throwJS(vm *goja.Runtime, err error) {
	ctor, ok := goja.AssertConstructor(vm.Get("Error"))
	if !ok {
		panic(vm.ToValue(err.Error()))
	}
	obj, cerr := ctor(nil, vm.ToValue(err.Error()))
	if cerr != nil {
		panic(vm.ToValue(err.Error()))
	}
	panic(obj)
}
