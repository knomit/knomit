package facts

import (
	"context"
	"testing"
)

// WriteFileExactForTest is writeFileExact for the parent's white-box tests,
// which write a file whose exact path they own (the ontology, a system file)
// through the same core WriteFact uses, with the same signer, validation and
// NotifyCommit. It panics outside a test binary, so no production path can
// reach the case-preserving core directly.
func (fi *Index) WriteFileExactForTest(ctx context.Context, branch, path, content, message, operation, expectBlob string) (commitHash string, blobHash string, err error) {
	mustBeTest("WriteFileExactForTest")
	return fi.writeFileExact(ctx, branch, path, content, message, operation, expectBlob)
}

// SetDeleteFaultForTest makes DeleteFact on this Index call f(path) first and
// return its error, without deleting anything, when f returns one. A nil f
// result lets the delete through. It lets a test drive the case where one
// retract fails while its neighbours succeed, which the write path never
// produces on its own. It affects this Index only, never another Service's.
// It panics outside a test binary; the returned func removes the hook.
func (fi *Index) SetDeleteFaultForTest(f func(path string) error) (restore func()) {
	mustBeTest("SetDeleteFaultForTest")
	fi.deleteFault.Store(&f)
	return func() { fi.deleteFault.Store(nil) }
}

// SetReadFactFaultForTest makes ReadFact on this Index call f(path, opts) first
// and return its error, without reading anything, when f returns one. opts is
// never nil (a nil argument has been defaulted), so f can tell a pinned read
// (AtCommit) from the walk-back one (BeforeCommit). A nil f result lets the read
// through. Every caller of ReadFact sees the fault, the write path's prior-refs
// read included. It affects this Index only, never another Service's. It panics
// outside a test binary; the returned func restores the previous hook.
//
// The hook closure may run on several goroutines at once (the lens fan-out
// reads every mount concurrently), so a closure that records anything must be
// goroutine-safe. Match on the exact path (and opts) and assert the injected error's
// identity (a distinct sentinel or message), not only a status, so a test
// cannot pass for the wrong reason.
func (fi *Index) SetReadFactFaultForTest(f func(path string, opts *ReadFactOpts) error) (restore func()) {
	mustBeTest("SetReadFactFaultForTest")
	prev := fi.readFactFault.Load()
	if f == nil {
		fi.readFactFault.Store(nil)
	} else {
		fi.readFactFault.Store(&f)
	}
	return func() { fi.readFactFault.Store(prev) }
}

// SetListDirFaultForTest makes ListDir on this Index call f(path) first and
// return its error, without listing anything, when f returns one. A nil f
// result lets the read through. It affects this Index only, never another
// Service's. It panics outside a test binary; the returned func restores the previous hook.
//
// The hook closure may run on several goroutines at once (the lens fan-out
// reads every mount concurrently), so a closure that records anything must be
// goroutine-safe. Match on the exact path (and opts) and assert the injected error's
// identity (a distinct sentinel or message), not only a status, so a test
// cannot pass for the wrong reason.
func (fi *Index) SetListDirFaultForTest(f func(path string) error) (restore func()) {
	mustBeTest("SetListDirFaultForTest")
	prev := fi.listDirFault.Load()
	if f == nil {
		fi.listDirFault.Store(nil)
	} else {
		fi.listDirFault.Store(&f)
	}
	return func() { fi.listDirFault.Store(prev) }
}

// SetWriteFaultForTest makes WriteFact on this Index call f(path) first and
// return its error, without writing anything, when f returns one. A nil f
// result lets the write through. It covers WriteFact only, not
// WriteFactIfUnchanged or the batch writers; DeleteFact has its own
// (SetDeleteFaultForTest). It affects this Index only, never another Service's.
// It panics outside a test binary; the returned func restores the previous hook.
//
// The hook closure may run on several goroutines at once (the lens fan-out
// reads every mount concurrently), so a closure that records anything must be
// goroutine-safe. Match on the exact path (and opts) and assert the injected error's
// identity (a distinct sentinel or message), not only a status, so a test
// cannot pass for the wrong reason.
func (fi *Index) SetWriteFaultForTest(f func(path string) error) (restore func()) {
	mustBeTest("SetWriteFaultForTest")
	prev := fi.writeFault.Load()
	if f == nil {
		fi.writeFault.Store(nil)
	} else {
		fi.writeFault.Store(&f)
	}
	return func() { fi.writeFault.Store(prev) }
}

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("facts." + name + " called outside a test binary")
	}
}
