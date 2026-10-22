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

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("facts." + name + " called outside a test binary")
	}
}
