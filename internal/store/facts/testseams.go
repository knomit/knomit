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

// serviceIndex maps a *store.Service, passed as any because this package cannot
// import the store, to the *Index it was built with. The store registers it
// from its init; storetest reads it through ServiceIndexForTest. Same shape and
// reason as vcs.ServiceRepoForTest: no Service accessor returns the handle.
var serviceIndex func(svc any) *Index

// RegisterServiceIndex installs the function ServiceIndexForTest uses. The
// store calls it once, from its init. A second registration panics.
func RegisterServiceIndex(f func(svc any) *Index) {
	if serviceIndex != nil {
		panic("facts: RegisterServiceIndex called twice")
	}
	serviceIndex = f
}

// ServiceIndexForTest is the *Index of svc, a *store.Service. It panics outside
// a test binary.
func ServiceIndexForTest(svc any) *Index {
	mustBeTest("ServiceIndexForTest")
	if serviceIndex == nil {
		panic("facts: ServiceIndexForTest called before the store registered its Service")
	}
	return serviceIndex(svc)
}

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("facts." + name + " called outside a test binary")
	}
}
