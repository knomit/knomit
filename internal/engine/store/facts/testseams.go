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
// outside a test binary; the returned func removes the hook.
func (fi *Index) SetReadFactFaultForTest(f func(path string, opts *ReadFactOpts) error) (restore func()) {
	mustBeTest("SetReadFactFaultForTest")
	if f == nil {
		fi.readFactFault.Store(nil)
	} else {
		fi.readFactFault.Store(&f)
	}
	return func() { fi.readFactFault.Store(nil) }
}

// SetListDirFaultForTest makes ListDir on this Index call f(path) first and
// return its error, without listing anything, when f returns one. A nil f
// result lets the read through. It affects this Index only, never another
// Service's. It panics outside a test binary; the returned func removes the hook.
func (fi *Index) SetListDirFaultForTest(f func(path string) error) (restore func()) {
	mustBeTest("SetListDirFaultForTest")
	if f == nil {
		fi.listDirFault.Store(nil)
	} else {
		fi.listDirFault.Store(&f)
	}
	return func() { fi.listDirFault.Store(nil) }
}

// SetWriteFaultForTest makes WriteFact on this Index call f(path) first and
// return its error, without writing anything, when f returns one. A nil f
// result lets the write through. It covers WriteFact only, not
// WriteFactIfUnchanged or the batch writers; DeleteFact has its own
// (SetDeleteFaultForTest). It affects this Index only, never another Service's.
// It panics outside a test binary; the returned func removes the hook.
func (fi *Index) SetWriteFaultForTest(f func(path string) error) (restore func()) {
	mustBeTest("SetWriteFaultForTest")
	if f == nil {
		fi.writeFault.Store(nil)
	} else {
		fi.writeFault.Store(&f)
	}
	return func() { fi.writeFault.Store(nil) }
}

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("facts." + name + " called outside a test binary")
	}
}
