package web

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// benchSink keeps benchMountWork's result live. Without an escaping store the
// function is pure and its result unused, and the compiler is free to delete
// the loop in one arm of the comparison but not the other — which produces a
// confident, meaningless speed-up.
var benchSink atomic.Uint64

// benchMountWork is a deterministic, CPU-bound stand-in for one mount's store
// work. Real per-mount cost is SQLite doing aggregates or a KNN scan; what
// matters to the fan-out is only that it is CPU-bound and independent, so a
// pure computation makes the scaling property measurable without a database.
func benchMountWork() {
	var acc uint64
	for i := uint64(0); i < 400_000; i++ {
		acc = acc*1664525 + 1013904223 + i
	}
	benchSink.Add(acc)
}

// benchTargets builds n fan-out targets backed by real (empty) store
// instances. Real ones rather than zero values because fanOutMounts names the
// mount when a leg panics, and a benchmark that would crash on the diagnostic
// path is not a safe thing to leave lying around.
func benchTargets(b *testing.B, n int) []federate.Target {
	b.Helper()
	dir := b.TempDir()
	out := make([]federate.Target, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("m%02d", i)
		svc, err := store.Open(filepath.Join(dir, name+".db"))
		if err != nil {
			b.Fatalf("open %s: %v", name, err)
		}
		b.Cleanup(func() { _ = svc.Close() })
		if err := svc.InitRepo(map[string]string{}, "agent/bench"); err != nil {
			b.Fatalf("init %s: %v", name, err)
		}
		ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
			Name:         name,
			UID:          name,
			AgentBranch:  "agent/bench",
			Svc:          svc,
			Ontology:     fact.CodeOntology(),
			OntologyRoot: "kb",
		})
		out = append(out, federate.Target{RT: repos.ReadTarget{RI: ri, Branch: "agent/bench"}})
	}
	return out
}

// BenchmarkFanOutMounts measures what the serial loop cost and what the
// concurrent one costs, at the mount counts that matter: today's lens (5) and
// the tens-of-mounts scale this work exists for.
//
// This is the Go-level counterpart to the end-to-end lab numbers in the commit
// messages. The lab measures a real corpus but through curl, which carries
// process-spawn noise and cannot separate connection-pool contention from CPU
// saturation. This measures only the fan-out's own shape: identical independent
// per-mount work, serial against concurrent. Serial is expected to be linear in
// mount count and concurrent to flatten until it saturates the machine's cores
// — the whole reason a 40-mount lens is viable at all.
func BenchmarkFanOutMounts(b *testing.B) {
	for _, mounts := range []int{1, 5, 20, 40} {
		targets := benchTargets(b, mounts)

		b.Run(fmt.Sprintf("serial/mounts=%d", mounts), func(b *testing.B) {
			for b.Loop() {
				for range targets {
					benchMountWork()
				}
			}
		})

		b.Run(fmt.Sprintf("concurrent/mounts=%d", mounts), func(b *testing.B) {
			for b.Loop() {
				if f := fanOutMounts(targets, func(int, federate.Target) (string, error) {
					benchMountWork()
					return "", nil
				}); f != nil {
					b.Fatalf("unexpected failure: %v", f.Err)
				}
			}
		})
	}
}
