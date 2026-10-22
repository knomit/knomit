// Package storetest holds the test seams that tests outside package store use
// to reach a *store.Service's internals: corrupt an object, plant a ref, commit
// without the index, backdate an experiment, read the raw database. The Service
// itself carries no test-only method; each function here takes the *Service and
// does its work through the child package's own seam (vcs, facts, merge,
// sqlite). The vcs repo is reached through a registration hook guarded by
// mustBeTest; the facts index and merge repo are the handles Service.Facts and
// Service.Merge already hand out.
//
// This is a test package, not an API. Three things keep it out of production:
//
//   - init panics unless the binary is a test binary (testing.Testing), so a
//     binary that links it dies at start-up instead of shipping the doors;
//   - test/archtest keeps it out of every shipped binary's dependency graph
//     (harness isolation) and keeps production source from importing it or
//     calling any *ForTest or Testing* identifier (TestTestSeamsNotReachedFromProduction);
//   - every door it opens is itself refused outside a test binary by the
//     child's mustBeTest.
//
// Tests inside package store cannot import this package (storetest imports
// store); they call the child seams directly through s.rh.
package storetest

import "testing"

func init() {
	if !testing.Testing() {
		panic("storetest: imported outside a test binary")
	}
}
