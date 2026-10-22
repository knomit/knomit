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

// ReadName names a motif read SetReadFaultForTest can fail. It is a distinct
// type with one constant per faultable read, so a mistyped name is a compile
// error and not a fault that silently never fires.
type ReadName string

// The reads SetReadFaultForTest can fail. ClusterKey is not faultable.
const (
	ReadClustersUnder         ReadName = "ClustersUnder"
	ReadDefinitions           ReadName = "Definitions"
	ReadVocabularyHealthUnder ReadName = "VocabularyHealthUnder"
	ReadAliasRows             ReadName = "AliasRows"
)

// SetReadFaultForTest makes the motif reads ClustersUnder, Definitions,
// VocabularyHealthUnder and AliasRows on this MotifIndex call f(read) first and
// return its error, without reading anything, when f returns one. A nil f
// result lets the read through and a nil f removes the hook. It is FAIL-ONLY:
// it cannot fake an answer. It affects this MotifIndex only, that is one
// store.Service, never another's. It panics outside a test binary; the returned
// func restores the previous hook.
//
// The hook closure may run on several goroutines at once (the lens fan-out
// reads every mount concurrently), so a closure that records anything must be
// goroutine-safe. Match on the exact ReadName and assert the injected error's
// identity (a distinct sentinel or message), not only a status, so a test
// cannot pass for the wrong reason.
func (mi *MotifIndex) SetReadFaultForTest(f func(read ReadName) error) (restore func()) {
	mustBeTest("SetReadFaultForTest")
	prev := mi.fault.Load()
	if f == nil {
		mi.fault.Store(nil)
	} else {
		mi.fault.Store(&f)
	}
	return func() { mi.fault.Store(prev) }
}

// PairKeyForTest is pairKey: the canonical identity of an unordered motif
// cluster pair, the key the verdict table and the pair selector use.
func PairKeyForTest(a, b string) string {
	mustBeTest("PairKeyForTest")
	return pairKey(a, b)
}
