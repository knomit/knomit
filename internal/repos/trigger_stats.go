package repos

import (
	"sort"
	"sync"
	"time"
)

// triggerP95Window is how many recent durations per trigger the p95 is
// computed over. An exact since-start p95 would need every sample kept
// forever; count, min and max ARE exact since start. The value is shown next
// to the percentile as p95_window so a reader knows what "recent" means.
const triggerP95Window = 1024

// triggerStats is the per-repo, in-memory statistics of the trigger
// dispatcher (the user's requirement: "statistics per repo per trigger — how
// many times fired, min/max, p95"). Kept on the dispatcher, reset on restart
// like the metrics registry; the fire log is the durable record. Updated only
// by the dispatcher goroutine; REST reads take the same mutex and copy.
type triggerStats struct {
	mu       sync.Mutex
	triggers map[string]*triggerStat
	runs     int64
	last     *TriggerLastRun
}

// triggerStat is one trigger's counters. Durations are the trigger's OWN work
// only: its `if` plus its action. Building `change` (the toucher walk, the
// signature check, the trailer read) is knomit's per-path cost, timed
// separately as change_ms on the run, so a long merge walk never reads as a
// slow trigger.
type triggerStat struct {
	evaluations, fires, ifFalse, ifError, ifTimeout, unparseable, slow int64
	// the `do: script` outcomes (F07 PR 3); selfCaused counts fires the
	// loop guard skipped before `if` (not evaluations, no row)
	scriptError, scriptTimeout, rateLimited, selfCaused int64
	count                                               int64
	min, max                                            time.Duration
	recent                                              []time.Duration // ring of the last triggerP95Window
	recentPos                                           int
}

// TriggerLastRun is the last dispatcher run's numbers.
type TriggerLastRun struct {
	RangeFrom  string `json:"range_from"`
	RangeTo    string `json:"range_to"`
	Nonlinear  bool   `json:"nonlinear,omitempty"`
	Paths      int    `json:"paths"`
	Evaluated  int    `json:"evaluated"`
	Fires      int    `json:"fires"`
	DurationMS int64  `json:"duration_ms"`
	DiffMS     int64  `json:"diff_ms"`
	ChangeMS   int64  `json:"change_ms"`
}

// TriggerRunStats is the run-level part of the report.
type TriggerRunStats struct {
	Runs int64           `json:"runs"`
	Last *TriggerLastRun `json:"last,omitempty"`
}

// TriggerDurationStats is a trigger's `if`+action duration: count, min and
// max exact since start; p95 over the most recent p95_window evaluations.
type TriggerDurationStats struct {
	Count       int64   `json:"count"`
	MinMS       float64 `json:"min_ms"`
	MaxMS       float64 `json:"max_ms"`
	P95RecentMS float64 `json:"p95_recent_ms"`
	P95Window   int     `json:"p95_window"`
}

// TriggerStatsView is one trigger's statistics as the endpoint shows them.
type TriggerStatsView struct {
	Evaluations int64 `json:"evaluations"`
	Fires       int64 `json:"fires"`
	IfFalse     int64 `json:"if_false"`
	IfError     int64 `json:"if_error"`
	IfTimeout   int64 `json:"if_timeout"`
	Unparseable int64 `json:"unparseable"`
	// The `do: script` kinds: ScriptError (a throw, a refused write, a
	// missing or uncompilable script), ScriptTimeout (the budget),
	// RateLimited (dropped by the per-minute cap), SelfCaused (skipped by
	// the loop guard: the path's toucher was this trigger's own script).
	ScriptError   int64                `json:"script_error"`
	ScriptTimeout int64                `json:"script_timeout"`
	RateLimited   int64                `json:"rate_limited"`
	SelfCaused    int64                `json:"self_caused"`
	Slow          int64                `json:"slow"`
	Duration      TriggerDurationStats `json:"duration"`
}

func newTriggerStats() *triggerStats {
	return &triggerStats{triggers: map[string]*triggerStat{}}
}

func (s *triggerStats) stat(name string) *triggerStat {
	st := s.triggers[name]
	if st == nil {
		st = &triggerStat{}
		s.triggers[name] = st
	}
	return st
}

// record counts one (trigger, path) evaluation with its outcome and the
// duration of the trigger's own work.
func (s *triggerStats) record(name, outcome string, d time.Duration, slow bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stat(name)
	st.evaluations++
	switch outcome {
	case "emitted":
		st.fires++
	case "unparseable":
		st.fires++
		st.unparseable++
	case "if-false":
		st.ifFalse++
	case "if-error":
		st.ifError++
	case "if-timeout":
		st.ifTimeout++
	case "ran":
		st.fires++
	case "kicked":
		st.fires++
	case "script-error":
		st.scriptError++
	case "script-timeout":
		st.scriptTimeout++
	case "rate-limited":
		st.rateLimited++
	}
	if slow {
		st.slow++
	}
	if st.count == 0 || d < st.min {
		st.min = d
	}
	if d > st.max {
		st.max = d
	}
	st.count++
	if len(st.recent) < triggerP95Window {
		st.recent = append(st.recent, d)
	} else {
		st.recent[st.recentPos] = d
		st.recentPos = (st.recentPos + 1) % triggerP95Window
	}
}

// recordSelfCaused counts a fire the loop guard skipped: not an evaluation
// (no `if` ran), no duration, no row.
func (s *triggerStats) recordSelfCaused(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stat(name).selfCaused++
}

// recordRun records one completed dispatcher run.
func (s *triggerStats) recordRun(last TriggerLastRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs++
	l := last
	s.last = &l
}

// runCount is how many runs completed since start.
func (s *triggerStats) runCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs
}

// view copies one trigger's statistics; a trigger never evaluated reads all
// zeros (the zeros are shown on purpose).
func (s *triggerStats) view(name string) TriggerStatsView {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.triggers[name]
	if st == nil {
		return TriggerStatsView{Duration: TriggerDurationStats{P95Window: triggerP95Window}}
	}
	return TriggerStatsView{
		Evaluations: st.evaluations, Fires: st.fires, IfFalse: st.ifFalse, IfError: st.ifError,
		IfTimeout: st.ifTimeout, Unparseable: st.unparseable, Slow: st.slow,
		ScriptError: st.scriptError, ScriptTimeout: st.scriptTimeout, RateLimited: st.rateLimited, SelfCaused: st.selfCaused,
		Duration: TriggerDurationStats{
			Count:       st.count,
			MinMS:       ms(st.min),
			MaxMS:       ms(st.max),
			P95RecentMS: ms(p95(st.recent)),
			P95Window:   triggerP95Window,
		},
	}
}

// runView copies the run-level statistics.
func (s *triggerStats) runView() TriggerRunStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := TriggerRunStats{Runs: s.runs}
	if s.last != nil {
		l := *s.last
		out.Last = &l
	}
	return out
}

// p95 is the nearest-rank 95th percentile of the samples (0 when none).
func p95(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := (len(sorted)*95 + 99) / 100 // ceil(0.95 * n)
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
