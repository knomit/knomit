package fact

import (
	"fmt"
	"testing"
	"time"
)

func TestGlob_Semantics(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		// * is one segment and never crosses "/".
		{"tasks/*/inbox.md", "tasks/r/inbox.md", true},
		{"tasks/*/inbox.md", "tasks/r/s/inbox.md", false},
		{"tasks/*.md", "tasks/a.md", true},
		{"tasks/*.md", "tasks/a/b.md", false},
		{"tasks/a*z.md", "tasks/abcz.md", true},
		{"tasks/a*z.md", "tasks/a/z.md", false},
		// ** is zero or more whole segments.
		{"tasks/**", "tasks/a.md", true},
		{"tasks/**", "tasks/a/b/c.md", true},
		{"tasks/**/x.md", "tasks/x.md", true},
		{"tasks/**/x.md", "tasks/a/b/x.md", true},
		{"tasks/**/x.md", "tasks/a/b/y.md", false},
		{"tasks/**", "other/a.md", false},
		// ? is exactly one non-"/" character.
		{"tasks/?.md", "tasks/a.md", true},
		{"tasks/?.md", "tasks/ab.md", false},
		{"tasks/a?b.md", "tasks/a/b.md", false},
		// Dots are literal; matching is ASCII case-insensitive.
		{"tasks/inbox/mindev.local-8ef0cd32/*.md", "tasks/inbox/mindev.local-8ef0cd32/t.md", true},
		{"tasks/inbox/mindev.local-8ef0cd32/*.md", "tasks/inbox/mindevXlocal-8ef0cd32/t.md", false},
		{"Tasks/Inbox/*.MD", "tasks/INBOX/a.md", true},
		// The whole path must be consumed.
		{"tasks/a.md", "tasks/a.md/b", false},
		{"tasks", "tasks/a.md", false},
	}
	for _, c := range cases {
		g, err := compileGlob(c.pat)
		if err != nil {
			t.Fatalf("compileGlob(%q): %v", c.pat, err)
		}
		if got := g.Match(c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestGlob_CompileRefusals(t *testing.T) {
	for _, pat := range []string{
		"",
		"tasks//a.md",
		"/tasks/a.md",
		"tasks/",
		"tasks/a**/b.md",
		"tasks/.knomit/**",
		".knomit/triggers/*.js",
		"tasks/x/.private/*.md",
	} {
		if _, err := compileGlob(pat); err == nil {
			t.Errorf("compileGlob(%q) = nil error, want a refusal", pat)
		}
	}
}

// realShapedPaths returns n fact paths shaped like a real knowledge base's:
// topic, two or three category segments, an 8-hex id.
func realShapedPaths(n int) []string {
	topics := []string{"decisions", "invariants", "gotchas", "architecture", "conventions", "incidents"}
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s/store/area-%d/sub-%d/%08x.md", topics[i%len(topics)], i%17, i%5, i*2654435761)
	}
	return out
}

// realShapedGlobs returns n compiled globs of the kinds triggers use.
func realShapedGlobs(tb testing.TB, n int) []*glob {
	shapes := []string{
		"decisions/**",
		"invariants/store/*/sub-1/*.md",
		"tasks/*/inbox/mindev.local-8ef0cd32/*.md",
		"gotchas/**/????????.md",
		"architecture/store/area-3/**",
	}
	out := make([]*glob, n)
	for i := range out {
		g, err := compileGlob(shapes[i%len(shapes)])
		if err != nil {
			tb.Fatal(err)
		}
		out[i] = g
	}
	return out
}

// Matching must not allocate: per-call compilation or splitting the path is
// what this catches (the relative timing gate below cannot, since a per-call
// compile is linear in the trigger count too).
func TestGlob_MatchDoesNotAllocate(t *testing.T) {
	paths := realShapedPaths(1000)
	globs := realShapedGlobs(t, 50)
	allocs := testing.AllocsPerRun(5, func() {
		for _, p := range paths {
			for _, g := range globs {
				g.Match(p)
			}
		}
	})
	if allocs != 0 {
		t.Fatalf("matching 1,000 paths against 50 globs allocated %v times per run, want 0", allocs)
	}
}

func matchAll(paths []string, globs []*glob) int {
	n := 0
	for _, p := range paths {
		for _, g := range globs {
			if g.Match(p) {
				n++
			}
		}
	}
	return n
}

// The scaling is reported RELATIVE — 50 triggers against 5 triggers of the
// SAME mix of shapes, timed in the same process — never as an absolute number
// of milliseconds. Linear matching costs 10x; a quadratic rescan ~100x. It is
// report-only (see the body): the falsifiable guard is the zero-allocation
// test.
func TestGlob_MatchScalesLinearly(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("timing test: skipped under -short and -race")
	}
	paths := realShapedPaths(1000)
	five := realShapedGlobs(t, 5)
	fifty := realShapedGlobs(t, 50)
	best := func(globs []*glob) time.Duration {
		var min time.Duration
		for i := 0; i < 7; i++ {
			start := time.Now()
			for j := 0; j < 20; j++ {
				matchAll(paths, globs)
			}
			if d := time.Since(start); i == 0 || d < min {
				min = d
			}
		}
		return min
	}
	t5, t50 := best(five), best(fifty)
	ratio := float64(t50) / float64(t5)
	// REPORT-ONLY. The 15x gate flaked at 21x on windows-2025 CI runners (a
	// shared runner's timer jitter on a 5-trigger baseline of a few hundred
	// microseconds), and a ratio cannot catch the one regression that matters
	// anyway: a per-call glob compile is also linear in the trigger count.
	// TestGlob_MatchDoesNotAllocate is the falsifiable guard; this logs the
	// ratio so a run's output still shows the scaling.
	t.Logf("20 x 1,000 paths: 5 triggers %v, 50 triggers %v (%.1fx; linear is 10x)", t5, t50, ratio)
}

// BenchmarkTriggerMatchPerAdvance reports the matching cost of one advance:
// every diff row against every compiled trigger. The PR body carries its
// numbers.
func BenchmarkTriggerMatchPerAdvance(b *testing.B) {
	for _, rows := range []int{8, 100, 1000} {
		for _, triggers := range []int{0, 1, 10, 50} {
			paths := realShapedPaths(rows)
			globs := realShapedGlobs(b, triggers)
			b.Run(fmt.Sprintf("rows=%d/triggers=%d", rows, triggers), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					matchAll(paths, globs)
				}
			})
		}
	}
}
