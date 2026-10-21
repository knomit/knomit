package store

import "knomit/internal/store/remotesync"

// The repo's consensus branch, recorded (F08 PR D, reviewer blocker B1). The
// state and the rules (the meta key, the cache, the candidate scan, the one
// named default) live in internal/store/remotesync/consensus_branch.go on
// remotesync.Index; this file keeps the Service method with its contract.

// DefaultConsensusBranch is the ONE name knomit gives a consensus branch it
// has to CREATE when nobody named one and there is nothing to detect it from.
// It is defined in remotesync (the single literal; TestNoHardcodedBranchLiterals
// holds that) and re-exported here unchanged.
const DefaultConsensusBranch = remotesync.DefaultConsensusBranch

// UpstreamBranch is the repo's consensus branch name: the origin row's
// Remote.Branch when the repo has an origin, else the recorded one. Served
// HEAD, the local reconcile, skills, recipes and the consensus merger all key
// off this.
func (s *Service) UpstreamBranch() string { return s.ri.UpstreamBranch() }

// recordConsensusBranch persists name as this repo's consensus branch and
// caches it (remotesync.Index.RecordConsensusBranch).
func (s *Service) recordConsensusBranch(name string) { s.ri.RecordConsensusBranch(name) }

// recordedConsensusBranch reads the recorded name from the repo database.
func (s *Service) recordedConsensusBranch() string { return s.ri.RecordedConsensusBranch() }
