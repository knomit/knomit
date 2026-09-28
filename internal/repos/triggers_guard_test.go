package repos

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// triggersRepoAllowedCalls is EVERY call the dispatcher may make, keyed as in
// internal/store/changes_guard_test.go ("<receiver>.<method>", "?.<method>"
// for an expression receiver, the bare name for a function). An ALLOWLIST on
// purpose: the dispatcher never writes a fact, a ref or an object, and never
// calls back into the store's write path. Its only store WRITES are the three
// trigger tables, reached through RecordTriggerRuns and
// AdvanceTriggerWatermarks (bookmarks and due marks); everything else is a
// read (DueCandidates and DueMarks are the `on: due` sweep's two reads), the
// hub, the log, or pure computation. The one clock read that decides anything
// is d.clock (time.Now().UTC(), once per run, for the sweep); the others time
// durations. A call that is not here fails the test — add it only after
// checking it keeps that contract.
var triggersRepoAllowedCalls = map[string][]string{
	"triggers.go": {
		// the store: reads, and the three tables
		"d.ri.Acquire", "release", "svc.Triggers", "svc.Branches", "?.HeadCommit", "svc.UpstreamBranch", "svc.SignerFingerprint",
		"tr.OntologyAtCommit", "tr.TriggerWatermarks", "tr.IsAncestor", "tr.DiffFacts", "tr.TreeReader",
		"tr.UpstreamTip", "tr.AncestorSet", "tr.RecordTriggerRuns", "tr.AdvanceTriggerWatermarks",
		"tr.DueCandidates", "tr.DueMarks",
		"cr.tr.CommitInfo", "cr.tr.CommitSignerOf", "cr.trees.Toucher", "cr.trees.BlobAt", "trees.BlobAt", "?.TriggerWatermarks", "?.RecentTriggerFires",
		"ri.WithRead", "ri.Name",
		// the compiled triggers
		"d.cache.Get", "ct.OnEpisode", "ct.Matches", "p.trig.EvalIf",
		"fact.ReadVerifySettings", "fact.ParseFact", "fact.FactGlobal", "fact.ExpiresUnix",
		"store.TrailerValue",
		// the hub and the log
		"hub.broadcastTrigger", "log.Warn", "log.Error", "log.Info", "log.Debug", "?.Err", "?.Str", "?.Dur", "?.Int64", "?.Interface", "?.Msg",
		// the push action (F07 PR 4): a non-blocking send on the sync wake,
		// from phase B's own case — never a push, never the network
		"d.wakeSync", "d.ri.wakeSync",
		"crashdump.ReportRecovered",
		// the dispatcher's own state and helpers
		"newTriggerStats", "triggerIdentityFor", "isHex8", "currentTriggerHooks", "d.triggerKick", "d.loop", "d.safeRun",
		"d.run", "d.phaseA", "d.advance", "d.sweepDue", "d.phaseB", "d.buffer", "d.maybeFlush", "d.flush", "d.settle", "d.flushOnStop", "d.resetPending",
		"d.overlayPending", "d.verifyModeOn", "d.verifiedBelow", "d.buildChange", "d.emit", "d.recordSet",
		"d.lastCompiledSet", "d.logInvalidOnce", "d.logOntologyErrorOnce", "d.clearOntologyError", "d.clock",
		"d.stats.record", "d.stats.recordRun", "d.stats.recordSelfCaused", "d.stats.view", "d.stats.runView", "d.pending.empty", "rs.didWork",
		"cr.metaOf", "factGlobal", "nameStates", "episodeOf", "shortHash", "capForLog", "appendTrigger",
		"d.cancel", "cancel", "timer.Stop", "h",
		// the `do: script` half (trigger_script.go has its own list below):
		// the phase-A load, the phase-B run, the report overlay, the trace
		"newScriptState", "d.loadScripts", "d.runScript", "d.scriptError", "deriveTrace", "hostMS.Milliseconds",
		"ev.Str", "ev.Msg", "string",
		// sync and context
		"d.mu.Lock", "d.mu.Unlock", "d.wg.Add", "d.wg.Wait", "d.wg.Done", "triggerHooksMu.Lock", "triggerHooksMu.Unlock",
		"context.WithCancel", "context.WithTimeout", "context.Background", "ctx.Err", "ctx.Done",
		// pure helpers
		"time.Now", "time.Since", "time.Duration", "time.Sleep", "time.NewTimer", "?.Milliseconds", "?.UTC", "?.Truncate", "rs.now.Unix",
		"sha256.Sum256", "hex.EncodeToString", "signer.PublicKey", "?.Marshal",
		"strings.TrimPrefix", "strings.LastIndex", "strings.HasSuffix", "strings.ToLower",
		"plumbing.NewHash", "commit.String", "head.String", "errors.As", "err.Error", "fmt.Sprintf",
		"sort.Strings", "sort.Slice", "sort.SliceStable", "append", "len", "make", "delete", "recover",
	},
	// The script host: its ONLY way to the store is the injected ScriptTools
	// (the MCP handlers, which take the branch lock like any writer) and the
	// one tree read of the script (tr.ScriptAt); the hub for emit; the log;
	// pure computation. Nothing here writes a fact, a ref or an object.
	"trigger_script.go": {
		// the store: the script blob at the head, and the injected tool set
		"tr.ScriptAt", "?.Call",
		// the binding and the trailers the handlers see
		"NewBindingOfRepo", "WithBinding", "store.WithTrailers", "context.WithTimeout", "cancel", "ctx.Err", "hostCtx.Err",
		// the sandbox
		"fact.CompileScript", "fact.RunScript", "fact.TriggerScriptPath", "fact.NormalizePath", "fact.IsPrivatePath",
		// the hub (through emit) and the log
		"h.d.emit", "h.d.wakeSync", "log.Error", "log.Warn", "?.Str", "?.Int", "?.Dur", "?.Msg", "crashdump.ReportRecovered",
		// the dispatcher's own state and helpers
		"currentTriggerHooks", "d.scriptFor", "d.rateAllows", "d.warnOnce", "h.d.warnOnce", "h.functions", "h.call", "h.momentName", "h.factPath",
		"d.mu.Lock", "d.mu.Unlock", "argAt", "stringArg", "objectArg", "capForLog",
		// pure helpers
		"time.Now", "time.Since", "now.Add", "?.After", "json.Marshal", "json.Unmarshal", "strings.TrimSpace",
		"errors.Is", "errors.As", "errors.New", "fmt.Errorf", "fmt.Sprintf", "err.Error", "cs.err.Error",
		"append", "len", "delete", "string", "recover",
	},
}

// TestTriggers_RepoCallsAreAllowlisted is the dispatcher's structural
// no-write guard. Sabotage: add a WriteFact (or any store write beyond the
// two tables) to triggers.go — the call is not on the list.
func TestTriggers_RepoCallsAreAllowlisted(t *testing.T) {
	fset := token.NewFileSet()
	for f, list := range triggersRepoAllowedCalls {
		allowed := map[string]bool{}
		for _, c := range list {
			allowed[c] = true
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err, f)
		seen := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			key := callKeyOf(call)
			if key == "" {
				return true
			}
			seen[key] = true
			if !allowed[key] {
				t.Errorf("%s: call %s is not on the dispatcher allowlist — the dispatcher writes nothing but its two tables "+
					"and never reaches the store's write path; check it and add it only if it keeps that contract",
					fset.Position(call.Pos()), key)
			}
			return true
		})
		var unused []string
		for c := range allowed {
			if !seen[c] {
				unused = append(unused, c)
			}
		}
		sort.Strings(unused)
		require.Empty(t, unused, "%s: allowlist entries no longer called — remove them", f)
		require.Greater(t, len(seen), 40, "%s: the guard must actually have walked the file", f)
	}
}

func callKeyOf(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		switch x := fn.X.(type) {
		case *ast.Ident:
			return x.Name + "." + fn.Sel.Name
		case *ast.SelectorExpr:
			// two-level receivers (d.stats.record, d.mu.Lock, cr.trees.Toucher)
			if root, ok := x.X.(*ast.Ident); ok {
				return root.Name + "." + x.Sel.Name + "." + fn.Sel.Name
			}
		}
		return "?." + fn.Sel.Name
	}
	return ""
}
