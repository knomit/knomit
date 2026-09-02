package textnorm

import "testing"

// The measurement behind Stem's memo. Run:
//
//	go test ./internal/textnorm/ -run XXX -bench 'Stem|Tokens' -benchtime 2000x
//
// On an Apple M4 (2026-09-01), rounded to the precision that reproduces —
// the warm figures are tens of nanoseconds and move run to run, so read the
// ORDER, never the digit, and do not treat a differing last digit as a
// regression:
//
//	BenchmarkStemCold      ~25 µs/op    ~480 B/op   13 allocs/op
//	BenchmarkStem         tens of ns      0 B/op    0 allocs/op
//	BenchmarkTokensCold    ~45 µs/op   ~1450 B/op   35 allocs/op
//	BenchmarkTokens      hundreds of ns  128 B/op    2 allocs/op
//
// Three orders of magnitude on Stem is the result; the allocation counts are
// the part worth pinning exactly, and the hit path's zero is the one a change
// here must not move.
//
// Cold is what EVERY call cost before the memo, not a rare first-call price:
// nothing cached the pluralizer's answer, so the knomit_motif_key SQL callback
// paid it per token per motif per row on every request. End to end that was a
// five-mount lens vocabulary at ~133ms; with the memo it is ~9ms.
//
// The cold benchmarks clear the memo per iteration OUTSIDE the timer, so they
// measure go-pluralize and not map churn. One of Cold's allocations is the
// strings.Clone that keeps an entry from pinning its caller's buffer; it lands
// on the miss, and the hit path stays at zero allocations.

var benchToken = "vulnerabilities"

var benchCanonical = Canonicalize("failure-presents-as-success")

var stemSink string

var tokensSink []string

func BenchmarkStem(b *testing.B) {
	stemSink = Stem(benchToken) // warm
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		stemSink = Stem(benchToken)
	}
}

func BenchmarkStemCold(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		resetStemMemo()
		b.StartTimer()
		stemSink = Stem(benchToken)
	}
}

func BenchmarkTokens(b *testing.B) {
	tokensSink = Tokens(benchCanonical) // warm
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		tokensSink = Tokens(benchCanonical)
	}
}

func BenchmarkTokensCold(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		resetStemMemo()
		b.StartTimer()
		tokensSink = Tokens(benchCanonical)
	}
}
