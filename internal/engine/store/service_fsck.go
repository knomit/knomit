package store

import (
	"context"

	"knomit/internal/engine/store/fsck"
)

// integrity returns the integrity checker over this Service's repository
// handler and indices (fsck/integrity.go). The checks take these three pieces rather
// than the Service so the checker does not depend on the facade.
func (s *Service) integrity() *fsck.Checker {
	return fsck.NewChecker(s.rh.Repo, s.fi, s.si)
}

// Verify walks the store and returns an integrity report. See integrity.Verify.
func (s *Service) Verify(ctx context.Context, opts fsck.VerifyOpts) (fsck.IntegrityReport, error) {
	return s.integrity().Verify(ctx, opts)
}

// PruneGeneratedRefs deletes generated export residue. See
// integrity.PruneGeneratedRefs.
func (s *Service) PruneGeneratedRefs(ctx context.Context) (fsck.PruneResult, error) {
	return s.integrity().PruneGeneratedRefs(ctx)
}
