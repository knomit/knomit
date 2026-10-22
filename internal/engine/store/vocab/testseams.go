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

// SetReadFaultForTest makes the motif reads ClustersUnder, Definitions,
// VocabularyHealthUnder and AliasRows on this MotifIndex call f(method) first
// and return its error, without reading anything, when f returns one; method is
// the read's own name. A nil f result lets the read through and a nil f removes
// the hook. It is FAIL-ONLY: it cannot fake an answer. ClusterKey is not
// faultable. It affects this MotifIndex only, that is one store.Service, never
// another's. It panics outside a test binary; the returned func removes the hook.
func (mi *MotifIndex) SetReadFaultForTest(f func(method string) error) (restore func()) {
	mustBeTest("SetReadFaultForTest")
	if f == nil {
		mi.fault.Store(nil)
	} else {
		mi.fault.Store(&f)
	}
	return func() { mi.fault.Store(nil) }
}

// PairKeyForTest is pairKey: the canonical identity of an unordered motif
// cluster pair, the key the verdict table and the pair selector use.
func PairKeyForTest(a, b string) string {
	mustBeTest("PairKeyForTest")
	return pairKey(a, b)
}
