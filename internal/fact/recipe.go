package fact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// A recipe (F07 PR 5) is the TRUSTED tier: JavaScript that may start
// programs. It runs in the same sandbox VM as a script (no process, no
// require, `Date` pinned) with a DIFFERENT host map — the script's plus
// `exec`, built by the dispatcher (internal/repos/trigger_recipe.go) — and
// where it comes from is what makes it trusted: `.knomit/recipes/<name>.js`
// at the tip of the repo's main branch, else `<home>/recipes/<name>.js` on
// this machine (user ruling D1: "recipes run only from main. No enforcement
// on who actually adds the recipes in main for now"). This file is the
// sandbox half: the path, the name rule, the header, the run.

// Recipe defaults, used for a header field that is absent (D8: limits live in
// the recipe file, never in knomit.toml or the ontology).
const (
	RecipeDefaultConcurrent = 1
	RecipeDefaultTimeout    = 30 * time.Minute
)

// recipeHeaderPrefix starts the one-line header a recipe may carry on its
// FIRST line: `// knomit: {"concurrent": 2, "jitter_ms": 500, "timeout_ms": 1800000}`.
const recipeHeaderPrefix = "// knomit:"

// Recipe statuses a recipe may return as its completion value (R3).
const (
	RecipeStatusDone        = "done"
	RecipeStatusSpawned     = "spawned"
	RecipeStatusDelivered   = "delivered"
	RecipeStatusUnreachable = "unreachable"
	RecipeStatusError       = "error"
)

// TriggerRecipePath is the repo path of a recipe: the name (kebab-case,
// validated) directly under .knomit/recipes/.
func TriggerRecipePath(name string) string {
	return PrivateRoot + "/recipes/" + name + ".js"
}

// ValidRecipeName is the rule a recipe name must follow wherever it comes
// from (a trigger's `recipe:`, or knomit.run(name) at call time): lowercase
// kebab-case, so it names exactly one file directly under a recipes folder.
func ValidRecipeName(name string) bool { return validKeyRe.MatchString(name) }

// RecipeLimits is a recipe's header: concurrent runs per machine, the jitter
// slept before a run, and the recipe's whole budget.
type RecipeLimits struct {
	Concurrent int
	Jitter     time.Duration
	Timeout    time.Duration
}

// ParseRecipeHeader reads the optional header on the recipe's FIRST line. It
// is parsed as JSON, never executed; an unknown field, a malformed object or
// an out-of-range value is an error (the recipe is then a `recipe-error`).
// Absent fields (or no header) take the defaults.
func ParseRecipeHeader(src string) (RecipeLimits, error) {
	lim := RecipeLimits{Concurrent: RecipeDefaultConcurrent, Timeout: RecipeDefaultTimeout}
	first, _, _ := strings.Cut(src, "\n")
	first = strings.TrimSpace(strings.TrimPrefix(first, "\uFEFF"))
	if !strings.HasPrefix(first, recipeHeaderPrefix) {
		return lim, nil
	}
	var h struct {
		Concurrent *int   `json:"concurrent"`
		JitterMS   *int64 `json:"jitter_ms"`
		TimeoutMS  *int64 `json:"timeout_ms"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(strings.TrimSpace(strings.TrimPrefix(first, recipeHeaderPrefix)))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return lim, fmt.Errorf("recipe header: %w", err)
	}
	if h.Concurrent != nil {
		if *h.Concurrent < 1 {
			return lim, fmt.Errorf("recipe header: concurrent must be at least 1")
		}
		lim.Concurrent = *h.Concurrent
	}
	if h.JitterMS != nil {
		if *h.JitterMS < 0 {
			return lim, fmt.Errorf("recipe header: jitter_ms must not be negative")
		}
		lim.Jitter = time.Duration(*h.JitterMS) * time.Millisecond
	}
	if h.TimeoutMS != nil {
		if *h.TimeoutMS < 1 {
			return lim, fmt.Errorf("recipe header: timeout_ms must be at least 1")
		}
		lim.Timeout = time.Duration(*h.TimeoutMS) * time.Millisecond
	}
	return lim, nil
}

// CompileRecipe compiles a recipe's source once (strict mode) and parses its
// header. The dispatcher caches the result by the file's blob (main) or its
// content hash (local).
func CompileRecipe(name, src string) (*goja.Program, RecipeLimits, error) {
	lim, err := ParseRecipeHeader(src)
	if err != nil {
		return nil, lim, err
	}
	prog, err := goja.Compile("recipe "+name, src, true)
	if err != nil {
		return nil, lim, err
	}
	return prog, lim, nil
}

// RecipeResult is a recipe's completion value, validated: {status, message?,
// id?} with a known status.
type RecipeResult struct {
	Status  string
	Message string
	ID      string
}

// RunRecipe runs one recipe in a fresh sandbox, exactly as RunScript runs a
// script (globals frozen, `knomit` from host, ctx interrupts the JavaScript),
// and returns its completion value — the program's last expression — as a
// RecipeResult. A value that is not an object with a known status is an
// error ("invalid result").
func RunRecipe(ctx context.Context, prog *goja.Program, name string, globals map[string]any, host ScriptHost, now time.Time) (RecipeResult, error) {
	v, err := runSandboxed(ctx, prog, "recipe "+name, globals, host, now)
	if err != nil {
		return RecipeResult{}, err
	}
	out, err := plainValue(v)
	if err != nil {
		return RecipeResult{}, fmt.Errorf("recipe %s: invalid result: %w", name, err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		return RecipeResult{}, fmt.Errorf("recipe %s: invalid result: want an object {status, message?, id?}, got %T", name, out)
	}
	st, _ := m["status"].(string)
	switch st {
	case RecipeStatusDone, RecipeStatusSpawned, RecipeStatusDelivered, RecipeStatusUnreachable, RecipeStatusError:
	default:
		return RecipeResult{}, fmt.Errorf("recipe %s: invalid result: status %q is not one of done, spawned, delivered, unreachable, error", name, st)
	}
	r := RecipeResult{Status: st}
	if msg, ok := m["message"]; ok && msg != nil {
		r.Message = fmt.Sprint(msg)
	}
	if id, ok := m["id"]; ok && id != nil {
		r.ID = fmt.Sprint(id)
	}
	return r, nil
}

// DeactivateTriggerDoForTest makes do a RESERVED value until restore is
// called: its triggers compile as `unsupported`, as they would under an older
// binary. Since F07 PR 5 no ontology value is reserved, and this is how the
// unsupported state (no bookmark, no back-fill on activation) stays tested.
// Tests only; not safe to call concurrently with compilation.
func DeactivateTriggerDoForTest(do string) (restore func()) {
	activeDoMu.Lock()
	was := activeTriggerDo[do]
	delete(activeTriggerDo, do)
	activeDoMu.Unlock()
	return func() {
		activeDoMu.Lock()
		if was {
			activeTriggerDo[do] = true
		}
		activeDoMu.Unlock()
	}
}
