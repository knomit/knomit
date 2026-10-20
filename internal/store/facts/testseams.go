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

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("facts." + name + " called outside a test binary")
	}
}
