package trigger

import (
	"context"
	"testing"
	"time"

	"knomit/internal/domain/fact"
)

// Test seams for the repos parent's white-box tests. The trigger tests drive a
// real Manager and RepoInstance (a commit kicks the dispatcher through
// ri.onCommit), so they live in the parent and cannot reach this package's
// unexported state; these doors are what they reach instead. Each panics
// outside a test binary (testing.Testing), so no production path can use one,
// and test/archtest's B2 guard keeps production source from naming them.

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("trigger." + name + " called outside a test binary")
	}
}

// RecipeWaitDelayForTest is recipeWaitDelay: how long an ended exec call
// waits for its pipes after the child exits or is killed.
const RecipeWaitDelayForTest = recipeWaitDelay

// RecipeOutputCapForTest is recipeOutputCap: the cap on a recipe child's
// stdout and stderr each.
const RecipeOutputCapForTest = recipeOutputCap

// P95WindowForTest is triggerP95Window: how many recent durations a trigger's
// p95 is computed over.
const P95WindowForTest = triggerP95Window

// SetTestHooksForTest installs h as every dispatcher's test hooks (process
// wide); the zero TestHooks removes them.
func SetTestHooksForTest(h TestHooks) {
	mustBeTest("SetTestHooksForTest")
	triggerHooksMu.Lock()
	triggerTestHooks = h
	triggerHooksMu.Unlock()
}

// HooksForTest returns the installed test hooks, so a test can add one
// to those already in force.
func HooksForTest() TestHooks {
	mustBeTest("HooksForTest")
	return currentTriggerHooks()
}

// SetRecipeAfterStartForTest installs f to run right after a recipe's child
// is in its kill group (process group or job object); nil removes it.
func SetRecipeAfterStartForTest(f func(pid int)) {
	mustBeTest("SetRecipeAfterStartForTest")
	recipeExecHooksMu.Lock()
	recipeExecHooks.afterStart = f
	recipeExecHooksMu.Unlock()
}

// SetRecipeServerAddrWaitForTest replaces how long a recipe's exec waits for
// the server's own address, and returns the restore.
func SetRecipeServerAddrWaitForTest(d time.Duration) (restore func()) {
	mustBeTest("SetRecipeServerAddrWaitForTest")
	old := recipeServerAddrWait
	recipeServerAddrWait = d
	return func() { recipeServerAddrWait = old }
}

// RecipeMCPForTest is recipeMCP: the `mcp` global a recipe gets for repo
// (the stdio MCP config `kb claude init` writes, the server key, the
// allowed-tools form).
func RecipeMCPForTest(repo string) map[string]any {
	mustBeTest("RecipeMCPForTest")
	return recipeMCP(repo)
}

// RunSequenceForTest is runSequence: the head the last completed run read
// and how many runs have completed.
func (d *Dispatcher) RunSequenceForTest() (completedHead string, seq int64) {
	mustBeTest("RunSequenceForTest")
	return d.runSequence()
}

// FlushedForTest is flushed: whether phase C's buffer is empty.
func (d *Dispatcher) FlushedForTest() bool {
	mustBeTest("FlushedForTest")
	return d.flushed()
}

// StatsViewForTest is one trigger's statistics as the report shows them.
func (d *Dispatcher) StatsViewForTest(name string) StatsView {
	mustBeTest("StatsViewForTest")
	return d.stats.view(name)
}

// RunStatsForTest is the run summary as the report shows it.
func (d *Dispatcher) RunStatsForTest() RunStats {
	mustBeTest("RunStatsForTest")
	return d.stats.runView()
}

// RunOnceForTest runs one dispatcher pass (phases A and B, phase C buffered)
// on the caller's goroutine, and FlushForTest writes the buffer: the
// benchmark drives the passes itself.
func (d *Dispatcher) RunOnceForTest(ctx context.Context) {
	mustBeTest("RunOnceForTest")
	d.runOnce(ctx)
}

// FlushForTest is flush: phase C's two transactions.
func (d *Dispatcher) FlushForTest(ctx context.Context) {
	mustBeTest("FlushForTest")
	d.flush(ctx)
}

// IndexIsReadyForTest is indexIsReady: whether the due sweep may read the
// index now.
func (d *Dispatcher) IndexIsReadyForTest() bool {
	mustBeTest("IndexIsReadyForTest")
	return d.indexIsReady()
}

// ScriptCompilesForTest is how many script compiles the dispatcher has done
// (read under its lock).
func (d *Dispatcher) ScriptCompilesForTest() int {
	mustBeTest("ScriptCompilesForTest")
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sc.compiles
}

// HomeForTest is the knomit home the dispatcher was built with (its
// recipes/ folder holds the local recipes).
func (d *Dispatcher) HomeForTest() string {
	mustBeTest("HomeForTest")
	return d.home
}

// SlowForTest is the slow-trigger threshold the dispatcher was built with.
func (d *Dispatcher) SlowForTest() time.Duration {
	mustBeTest("SlowForTest")
	return d.slow
}

// IdentityForTest is the {agent} and {host} the dispatcher was built with.
func (d *Dispatcher) IdentityForTest() fact.TriggerIdentity {
	mustBeTest("IdentityForTest")
	return d.identity
}
