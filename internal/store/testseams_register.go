package store

import (
	"knomit/internal/store/facts"
	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/merge"
)

// The test-seam registration hook. storetest (internal/store/storetest) lives
// outside package store, so it cannot read Service.rh or Service.fi, and the
// Service must not grow an accessor that returns them: a value of an
// internal/ type handed out of the store crosses the internal rule even though
// the import does not. Instead the store registers, once, how to map a
// *Service to each child handle; each child's getter (vcs.ServiceRepoForTest,
// facts.ServiceIndexForTest, merge.ServiceRepoForTest) refuses outside a test
// binary. The functions take the Service as any because the children cannot
// import the store. Nothing here is reachable from production: the registered
// closures only run behind a getter that panics unless testing.Testing().
func init() {
	vcs.RegisterServiceRepo(func(svc any) *vcs.Repo { return svc.(*Service).rh.Repo })
	facts.RegisterServiceIndex(func(svc any) *facts.Index { return svc.(*Service).fi })
	merge.RegisterServiceRepo(func(svc any) *merge.Repo { return svc.(*Service).rh.mergeRepo })
}
