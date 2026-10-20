package vocab

import "testing"

// Test seams for the parent's white-box tests, which drive the vocab child
// through a full store.Service. Each panics outside a test binary
// (testing.Testing), so no production path can use the door.

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("vocab." + name + " called outside a test binary")
	}
}

// PairKeyForTest is pairKey: the canonical identity of an unordered motif
// cluster pair, the key the verdict table and the pair selector use.
func PairKeyForTest(a, b string) string {
	mustBeTest("PairKeyForTest")
	return pairKey(a, b)
}
