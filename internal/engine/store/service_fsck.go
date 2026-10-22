package store

import "knomit/internal/engine/store/fsck"

// Integrity returns the integrity checker over this Service's repository
// handler and indices (fsck/integrity.go): Verify walks the store and returns
// an integrity report, PruneGeneratedRefs deletes generated export residue.
// The checker takes these three pieces rather than the Service so it does not
// depend on the facade; callers cannot build one themselves because the
// repository handler is internal to the store. It is built per call.
func (s *Service) Integrity() *fsck.Checker {
	return fsck.NewChecker(s.rh.Repo, s.fi, s.si)
}
