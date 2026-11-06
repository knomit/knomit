package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ToolWindow is the adaptive tool list of a provider with no prompt cache to
// protect (Caps.StablePrefix false). The Builder keeps the complete tool list
// and the whole history; the loop sends Window.Project(Builder.Snapshot()), a
// copy in which hook 1 is the loader tool, the tools loaded so far in full and
// a STUB for every other catalogue tool (its name, its one-line purpose and a
// permissive schema). The model loads a tool by calling the loader
// (load_tools) or by calling its stub, which is not run but loads the tool;
// loaded tools fall out again (back to a stub), least recently used first, only
// when a request would not fit.
//
// The window is state; Project is a pure view. Nothing here edits the Builder,
// so ErrHistoryAppendOnly stays enforced (invariant 9351d681 / 80e22727).
// Loading never approves anything: the loader runs in the loop before the
// approval gate because it calls no tool and writes nothing, as is a call to a
// stub (never run); every call to a loaded tool that is run goes through
// CallGate.Decide, except an identical repeat of a call that just succeeded,
// which the no-progress guard answers from the earlier result without running
// it (invariant 9b26dd4b).
//
// Not safe for concurrent use, like Loop.

// LoadToolsName is the loader tool's name. It carries no knomit_ namespace so it
// cannot collide with a server tool; NewToolWindow refuses a catalogue that
// holds it.
const LoadToolsName = "load_tools"

// Loader and result copy: approved by the user (design section 7, C1 to C5).
// The model reads these, so they are constants, not inline strings.
const (
	// LoaderDescription starts the loader's description; the name catalogue follows.
	LoaderDescription = "Some knomit tools are listed with only a short description; their full definitions are not loaded yet. Call this with the names of the tools you need and their full definitions are added so you can call them in your next step. Calling a listed tool directly also loads it, but that call is not run, so call it again. This only loads definitions; it changes nothing in the knowledge base. Tool names:"
	// LoaderNamesParamDescription describes the loader's names parameter (C4).
	LoaderNamesParamDescription = `Tool names from the catalogue, for example ["knomit_query", "knomit_explain"].`

	// Loader results (C5). Each takes the arguments named in its comment.
	LoadedResult         = "Loaded: %s. Their definitions are available from your next step."              // names
	AlreadyLoadedResult  = "Already loaded: %s."                                                           // names
	NotInCatalogueResult = "Not in the catalogue: %s (did you mean %s?)."                                  // name, nearest
	NoRoomResult         = "Not loaded, no room: %s needs about %d tokens and about %d are free."          // name, needs, free
	EvictedResult        = "Unloaded to make room: %s. Load them again if you need them."                  // names
	NothingLoadedResult  = "Nothing was loaded. Catalogue names: %s."                                      // all catalogue names
	NotLoadedCallResult  = "%s was not loaded, so this call was not run. It is loaded now; call it again." // name

	// NotInCatalogueNoMatchResult is NotInCatalogueResult for a name with no near
	// match, so there is nothing to suggest. The user approved the suggestion
	// form only; this is the same sentence without the suggestion.
	NotInCatalogueNoMatchResult = "Not in the catalogue: %s." // name
)

// ReservedToolNameMessage is the error (user-visible, approved: design C10)
// for a server that offers a tool named like the loader. The argument is the
// loader's name.
const ReservedToolNameMessage = "The server already offers a tool named %s, which the chat reserves."

// Reasons a LoopToolsChanged event carries.
const (
	ToolsReasonPreload   = "preload"    // loaded by the ranker's guess before the turn's first request
	ToolsReasonModel     = "model"      // the model called the loader
	ToolsReasonStaleCall = "stale_call" // the model called a catalogue tool it had not loaded
	ToolsReasonPressure  = "pressure"   // evicted because the request would not fit
)

// ToolsChange is one change to the loaded set.
type ToolsChange struct {
	Loaded  []string // names added by this change
	Evicted []string // names removed by this change
	Reason  string
	// RankerError is set on a preload change whose ranker failed; the turn went
	// on with no preload. It is data for the log, never a user-facing error.
	RankerError string
	LoadedNow   []string // the loaded set after the change, sorted
	Tokens      int      // tokens (bytes/4) the loaded set adds to a request over the stubs it replaces, after the change
}

// Pinned is a set of tool names that must not be evicted.
type Pinned map[string]bool

// ToolWindowSpec builds a ToolWindow.
type ToolWindowSpec struct {
	// Loader is the loader tool's definition (see LoaderDef); its Name must be
	// LoadToolsName. Its description carries the catalogue, built by the edge.
	Loader ToolDef
	// Names are the catalogue's tool names: the tools the model may load.
	Names []string
	// Replayable says whether a call, made again with the same arguments,
	// returns the same data with no session effect; the stub of its result
	// offers a re-call only then. Nil means never (the edge passes
	// mcp.Replayable).
	Replayable func(tool string, args json.RawMessage) bool
	// Ranker, when set, guesses the tools a turn's message needs; Preload loads
	// the best guesses before the turn's first request. Nil: no preload.
	Ranker ToolRanker
	// Core are the catalogue tools that are always in the window: loaded from
	// the opening, never evicted, and not preload picks. A name that is not in
	// Names is ignored (the principal was not shown that tool).
	Core []string
	// Purposes are the one-line purposes the stubs carry as their descriptions,
	// by tool name (the approved catalogue lines). A tool with none gets a stub
	// with an empty description.
	Purposes map[string]string
}

// ToolWindow: see the file comment.
type ToolWindow struct {
	loader ToolDef
	names  []string // catalogue, sorted, distinct
	inCat  map[string]bool

	loaded      map[string]bool
	last        map[string]int // recency: the tick of the last load or call
	clock       int
	turnLoaded  Pinned // loaded since BeginTurn: never evicted in this turn
	batchLoaded Pinned // loaded since BeginBatch: a call to them in this batch is not run
	sizes       map[string]int
	bytes       map[string]int // definition bytes, for preload's rounding
	changes     []ToolsChange
	replayable  func(tool string, args json.RawMessage) bool
	ranker      ToolRanker
	picks       []string // tools preloaded this turn, in rank order
	core        map[string]bool
	purposes    map[string]string
	stubBytes   map[string]int // a catalogue tool's stub definition, bytes
}

// LoaderDef builds the loader tool's definition: the approved description with
// catalogue (the lines the edge built) appended, and the schema of the names
// parameter.
func LoaderDef(catalogue string) ToolDef {
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"names": map[string]any{
				"type":        "array",
				"minItems":    1,
				"items":       map[string]any{"type": "string"},
				"description": LoaderNamesParamDescription,
			},
		},
		"required":             []string{"names"},
		"additionalProperties": false,
	})
	canon, err := CanonicalJSON(schema)
	if err != nil {
		canon = schema
	}
	return ToolDef{Name: LoadToolsName, Description: LoaderDescription + "\n" + catalogue, Schema: canon}
}

// NewToolWindow builds a window with nothing loaded. It refuses a loader not
// named LoadToolsName and a catalogue that holds that name.
func NewToolWindow(spec ToolWindowSpec) (*ToolWindow, error) {
	if spec.Loader.Name != LoadToolsName {
		return nil, fmt.Errorf("llm: the loader tool must be named %q, got %q", LoadToolsName, spec.Loader.Name)
	}
	w := &ToolWindow{
		loader: spec.Loader, inCat: map[string]bool{},
		loaded: map[string]bool{}, last: map[string]int{},
		turnLoaded: Pinned{}, batchLoaded: Pinned{}, sizes: map[string]int{}, bytes: map[string]int{},
		replayable: spec.Replayable, ranker: spec.Ranker,
		purposes: spec.Purposes, stubBytes: map[string]int{},
	}
	for _, n := range spec.Names {
		if n == LoadToolsName {
			return nil, fmt.Errorf(ReservedToolNameMessage, LoadToolsName)
		}
		if !w.inCat[n] {
			w.inCat[n] = true
			w.names = append(w.names, n)
		}
	}
	sort.Strings(w.names)
	w.core = map[string]bool{}
	for _, n := range spec.Core {
		if w.inCat[n] {
			w.core[n] = true
			w.loaded[n] = true
		}
	}
	return w, nil
}

// Core is the core tools' names, sorted: always loaded, never evicted.
func (w *ToolWindow) Core() []string {
	out := make([]string, 0, len(w.core))
	for n := range w.core {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Observe learns the catalogue tools' definition sizes from snap and returns
// what is loaded and what the loaded set costs in tokens (bytes/4). It is for
// the opening, before any request has been projected: a chat shows the core
// set's size from its first turn.
func (w *ToolWindow) Observe(snap Snapshot) ([]string, int) {
	w.observe(snap)
	return w.loadedNames(), w.loadedTokens()
}

// Handles reports whether the window, not the approval gate and the host,
// answers a call to name: the loader, or a catalogue tool that is not loaded
// (or that this batch's loader just loaded: its arguments were written before
// the model saw the schema).
func (w *ToolWindow) Handles(name string) bool {
	if name == LoadToolsName {
		return true
	}
	return w.inCat[name] && (!w.loaded[name] || w.batchLoaded[name])
}

// BeginTurn starts a user turn: what was loaded earlier may be evicted again.
func (w *ToolWindow) BeginTurn() { w.turnLoaded = Pinned{}; w.picks = nil }

// BeginBatch starts the answering of one model response's tool calls.
func (w *ToolWindow) BeginBatch() { w.batchLoaded = Pinned{} }

// Touch records a call to name, making it the most recently used.
func (w *ToolWindow) Touch(name string) {
	if w.loaded[name] {
		w.clock++
		w.last[name] = w.clock
	}
}

// Loaded is the loaded tools' names, sorted.
func (w *ToolWindow) Loaded() []string { return w.loadedNames() }

// TakeChanges returns the changes since the last call and forgets them.
func (w *ToolWindow) TakeChanges() []ToolsChange {
	out := w.changes
	w.changes = nil
	return out
}

func (w *ToolWindow) loadedNames() []string {
	out := make([]string, 0, len(w.loaded))
	for n := range w.loaded {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// loadedTokens is what the loaded set adds to a request, in tokens (bytes/4):
// each loaded tool's full definition less the stub it replaces, which the
// request carries whether the tool is loaded or not.
func (w *ToolWindow) loadedTokens() int {
	n := 0
	for name := range w.loaded {
		n += w.growth(name)
	}
	return n
}

// observe remembers each catalogue tool's definition size (bytes/4) from snap.
func (w *ToolWindow) observe(snap Snapshot) {
	for _, it := range snap.Hooks[HookTools] {
		if it.Tool != nil {
			w.sizes[it.Tool.Name] = itemBytes(it) / 4
			w.bytes[it.Tool.Name] = itemBytes(it)
			w.stubBytes[it.Tool.Name] = itemBytes(w.toolStub(it.Tool.Name))
		}
	}
}

// stubSchema is the parameters schema of a stub: an object with no required
// field that allows any argument. It is the narrowest shape every provider
// takes (Ollama parses parameters into type/required/properties and ignores
// the rest; llama.cpp compiles any JSON schema), and it constrains nothing
// because a stub's call is never run.
const stubSchema = `{"additionalProperties":true,"properties":{},"type":"object"}`

// toolStub is the hook-1 item of a catalogue tool that is not loaded: its real
// name, the approved one-line purpose as the description, and stubSchema. A
// model that calls it gets NotLoadedCallResult and the tool is loaded.
func (w *ToolWindow) toolStub(name string) Item {
	d := ToolDef{Name: name, Description: w.purposes[name], Schema: json.RawMessage(stubSchema)}
	return Item{ID: "tool." + name, Hook: HookTools, Tool: &d}
}

// growth is what loading name adds to a request that already carries its stub,
// in tokens (bytes/4).
func (w *ToolWindow) growth(name string) int {
	return max(w.sizes[name]-w.stubBytes[name]/4, 0)
}

// growthBytes is growth in bytes, for preload's rounding.
func (w *ToolWindow) growthBytes(name string) int {
	return max(w.bytes[name]-w.stubBytes[name], 0)
}

// StubTokens is what the stubs of the tools that are not loaded cost in
// tokens (bytes/4), as of the last snapshot observed.
func (w *ToolWindow) StubTokens() int {
	n := 0
	for name := range w.inCat {
		if !w.loaded[name] {
			n += w.stubBytes[name]
		}
	}
	return n / 4
}

// Project returns snap with hook 1 replaced by the loader, the loaded tools
// with their full definitions and a stub for every other catalogue tool (all
// in snapshot order, which is by name). Every other hook is equal to snap's.
// It copies; snap and the Builder behind it are not touched.
func (w *ToolWindow) Project(snap Snapshot) Snapshot {
	w.observe(snap)
	var out Snapshot
	for h := HookTools; h <= HookTurn; h++ {
		if len(snap.Hooks[h]) > 0 {
			out.Hooks[h] = append([]Item(nil), snap.Hooks[h]...)
		}
	}
	loader := w.loader
	tools := []Item{{ID: "tool." + LoadToolsName, Hook: HookTools, Tool: &loader}}
	for _, it := range snap.Hooks[HookTools] {
		switch {
		case it.Tool == nil || !w.inCat[it.Tool.Name]:
		case w.loaded[it.Tool.Name]:
			tools = append(tools, it)
		default:
			tools = append(tools, w.toolStub(it.Tool.Name))
		}
	}
	out.Hooks[HookTools] = tools
	return out
}

// evictOrder lists the loaded tools that may be evicted, best victim first:
// least recently used, ties to the larger definition, then by name. Tools in
// pinned, tools the model loaded this turn and the loader are never listed.
// Preloaded picks are not pinned until called (the caller's pinned set holds
// the called ones), and unused picks are the first victims.
func (w *ToolWindow) evictOrder(pinned Pinned) []string {
	var cand []string
	for n := range w.loaded {
		if !pinned[n] && !w.turnLoaded[n] && !w.core[n] {
			cand = append(cand, n)
		}
	}
	guess := map[string]bool{}
	for _, n := range w.picks {
		guess[n] = true
	}
	sort.Slice(cand, func(i, j int) bool {
		a, b := cand[i], cand[j]
		// A preloaded pick the model has not called (a called one is in
		// pinned) is a guess: it goes before any tool the model asked for.
		if guess[a] != guess[b] {
			return guess[a]
		}
		if w.last[a] != w.last[b] {
			return w.last[a] < w.last[b]
		}
		if w.sizes[a] != w.sizes[b] {
			return w.sizes[a] > w.sizes[b]
		}
		return a < b
	})
	return cand
}

// EvictOne unloads the best victim and returns its name, or false when every
// loaded tool is pinned (or nothing is loaded). The caller evicts only between
// requests, when every earlier call of the turn has been answered.
func (w *ToolWindow) EvictOne(pinned Pinned) (string, bool) {
	order := w.evictOrder(pinned)
	if len(order) == 0 {
		return "", false
	}
	name := order[0]
	w.unload(name)
	w.record(nil, []string{name}, ToolsReasonPressure)
	return name, true
}

func (w *ToolWindow) unload(name string) {
	delete(w.loaded, name)
	delete(w.last, name)
}

func (w *ToolWindow) record(loaded, evicted []string, reason string) {
	w.changes = append(w.changes, ToolsChange{
		Loaded: loaded, Evicted: evicted, Reason: reason,
		LoadedNow: w.loadedNames(), Tokens: w.loadedTokens(),
	})
}

// admit loads name if the request still fits, evicting only as many unpinned
// tools as it takes. avail is the free room in tokens and is updated. It
// reports the evicted names, or ok=false and the tokens the tool needs.
func (w *ToolWindow) admit(name string, avail *int, pinned Pinned, tick int) (evicted []string, needs int, ok bool) {
	// *avail is measured with this tool's stub in the request; only the
	// growth from stub to full definition is new.
	needs = w.growth(name)
	if *avail < needs {
		freed := 0
		var victims []string
		for _, v := range w.evictOrder(pinned) {
			if *avail+freed >= needs {
				break
			}
			victims = append(victims, v)
			freed += w.growth(v) // evicted, it is a stub again, not gone
		}
		if *avail+freed < needs {
			return nil, needs, false
		}
		for _, v := range victims {
			w.unload(v)
		}
		*avail += freed
		evicted = victims
	}
	*avail -= needs
	w.loaded[name] = true
	w.last[name] = tick
	w.turnLoaded[name] = true
	w.batchLoaded[name] = true
	return evicted, needs, true
}

// Run answers a call the window handles (see Handles): the loader, or a call
// to a catalogue tool that is not loaded, which is NOT run but loads the tool.
// free reports the tokens the next request has room for; it may be negative,
// and is read once. pinned are tools that must stay (tools called in this turn,
// including this response's own calls); tools loaded this turn are pinned too.
// The result is the tool_result the model reads; IsError is true when nothing
// was loaded.
func (w *ToolWindow) Run(call ToolUse, snap Snapshot, free func() int, pinned Pinned) ToolOutcome {
	w.observe(snap)
	avail := free()
	w.clock++
	tick := w.clock
	if call.Name != LoadToolsName {
		return w.runStale(call.Name, &avail, pinned, tick)
	}

	var args struct {
		Names []string `json:"names"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil || len(args.Names) == 0 {
		return ToolOutcome{Content: fmt.Sprintf(NothingLoadedResult, strings.Join(w.names, ", ")), IsError: true}
	}

	var loaded, evicted, already, lines []string
	seen := map[string]bool{}
	for _, name := range args.Names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, known := w.sizes[name]; !w.inCat[name] || !known {
			lines = append(lines, w.unknownLine(name))
			continue
		}
		if w.loaded[name] {
			already = append(already, name)
			continue
		}
		ev, needs, ok := w.admit(name, &avail, pinned, tick)
		if !ok {
			lines = append(lines, fmt.Sprintf(NoRoomResult, name, needs, max(avail, 0)))
			continue
		}
		loaded = append(loaded, name)
		evicted = append(evicted, ev...)
	}

	var parts []string
	if len(loaded) > 0 {
		parts = append(parts, fmt.Sprintf(LoadedResult, strings.Join(loaded, ", ")))
	}
	if len(evicted) > 0 {
		parts = append(parts, fmt.Sprintf(EvictedResult, strings.Join(evicted, ", ")))
	}
	if len(already) > 0 {
		parts = append(parts, fmt.Sprintf(AlreadyLoadedResult, strings.Join(already, ", ")))
	}
	parts = append(parts, lines...)
	if len(loaded) > 0 {
		w.record(loaded, evicted, ToolsReasonModel)
		return ToolOutcome{Content: strings.Join(parts, "\n")}
	}
	if len(already) == 0 || len(lines) > 0 {
		parts = append(parts, fmt.Sprintf(NothingLoadedResult, strings.Join(w.names, ", ")))
	}
	return ToolOutcome{Content: strings.Join(parts, "\n"), IsError: true}
}

// runStale answers a call to a catalogue tool the model had not loaded: not
// run (its arguments were written without the schema), loaded for the next
// request.
func (w *ToolWindow) runStale(name string, avail *int, pinned Pinned, tick int) ToolOutcome {
	if _, known := w.sizes[name]; !w.inCat[name] || !known {
		return ToolOutcome{Content: w.unknownLine(name), IsError: true}
	}
	if w.loaded[name] { // loaded earlier in this batch
		return ToolOutcome{Content: fmt.Sprintf(NotLoadedCallResult, name), IsError: true}
	}
	ev, needs, ok := w.admit(name, avail, pinned, tick)
	if !ok {
		return ToolOutcome{Content: fmt.Sprintf(NoRoomResult, name, needs, max(*avail, 0)), IsError: true}
	}
	w.record([]string{name}, ev, ToolsReasonStaleCall)
	return ToolOutcome{Content: fmt.Sprintf(NotLoadedCallResult, name), IsError: true}
}

// unknownLine names a name that is not in the catalogue, with the catalogue
// names that contain it or that it contains (so "query" finds "knomit_query").
func (w *ToolWindow) unknownLine(name string) string {
	var near []string
	if low := strings.ToLower(strings.TrimSpace(name)); low != "" {
		for _, n := range w.names {
			ln := strings.ToLower(n)
			if strings.Contains(ln, low) || strings.Contains(low, ln) {
				near = append(near, n)
			}
		}
	}
	if len(near) == 0 {
		return fmt.Sprintf(NotInCatalogueNoMatchResult, name)
	}
	return fmt.Sprintf(NotInCatalogueResult, name, strings.Join(near, ", "))
}

// unboundedRoom is what a free func returns when the window is unknown.
const unboundedRoom = math.MaxInt / 4

// PreloadDivisor sets the preload budget P = usable room / PreloadDivisor.
// Policy fraction, not a measurement: the guess is a guess, so the most a wrong
// one may cost is an eighth of the usable room whatever the window (regret
// bound; design 4.4), and pressure eviction removes it later.
const PreloadDivisor = 8

// PreloadBudget is P in tokens for usable = window - reply reserve.
func PreloadBudget(usable int) int {
	if usable <= 0 {
		return 0
	}
	return usable / PreloadDivisor
}

// Picks are the tools Preload loaded this turn, in rank order.
func (w *ToolWindow) Picks() []string { return append([]string(nil), w.picks...) }

// Preload ranks the catalogue against the turn's message and loads the best
// guesses before the first request. Tools are taken greedily in rank order
// while their definition tokens (bytes/4) sum to at most min(P, free), P =
// PreloadBudget(usable); the first tool that does not fit ends it (a cheap
// low-ranked tool never jumps a big high-ranked one). Loaded tools cost
// nothing and are skipped. Preload only adds: it never evicts, so it cannot
// push a request over budget, and when nothing fits it loads nothing. A pick is
// pinned only once the model calls it this turn; until then it is the first
// thing pressure evicts (a wrong guess must not cost the turn).
//
// Tools tied with the lowest score carry no evidence (a message naming
// nothing scores every tool alike) and are not candidates; this is relative to
// the ranking, not a similarity cutoff. A ranker error is recorded on the
// change and swallowed. free is the tokens the next request has room for.
func (w *ToolWindow) Preload(ctx context.Context, message string, snap Snapshot, usable, free int) []string {
	w.picks = nil
	if w.ranker == nil {
		return nil
	}
	w.observe(snap)
	ranked, err := w.ranker.Rank(ctx, message)
	if err != nil {
		w.changes = append(w.changes, ToolsChange{Reason: ToolsReasonPreload, RankerError: err.Error(), LoadedNow: w.loadedNames(), Tokens: w.loadedTokens()})
		return nil
	}
	if len(ranked) < 2 {
		return nil
	}
	floor := ranked[0].Score
	for _, s := range ranked {
		floor = math.Min(floor, s.Score)
	}
	budget := min(PreloadBudget(usable), free)
	var loaded []string
	seen := map[string]bool{}
	for _, s := range ranked {
		if s.Score <= floor {
			break
		}
		name := s.Name
		_, known := w.sizes[name]
		if !w.inCat[name] || !known || seen[name] {
			continue
		}
		seen[name] = true
		if w.loaded[name] {
			continue
		}
		// Round up per tool: Snapshot.Record() floors the whole hook, so the
		// real growth of the estimate is at most the sum of the ceilings.
		size := (w.growthBytes(name) + 3) / 4
		if size > budget {
			break
		}
		budget -= size
		w.clock++
		w.loaded[name] = true
		w.last[name] = w.clock
		loaded = append(loaded, name)
	}
	if len(loaded) > 0 {
		w.picks = loaded
		w.record(loaded, nil, ToolsReasonPreload)
	}
	return loaded
}
