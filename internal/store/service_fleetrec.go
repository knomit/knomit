package store

import (
	"time"

	"golang.org/x/crypto/ssh"

	"knomit/internal/store/fleetrec"
)

// The Service's fleet readers: each reads this store (a mounted fleet
// repository) through the fleetrec child, which owns the reads themselves.

// FleetMembersAt returns the member records at branch in THIS store (a
// mounted fleet repository), after checking that its ontology there is the
// fleet ontology. root is the store's fact root.
func (s *Service) FleetMembersAt(branch string) ([]fleetrec.Member, error) {
	f, err := s.FleetAt(branch)
	return f.Members, err
}

// FleetAt returns everything the judge reads from THIS store (a mounted fleet
// repository) at branch, all from the ONE commit branch names: the member
// records and the certificate artifacts. Reading them separately could pair
// the records of one revision with the CRL of another.
func (s *Service) FleetAt(branch string) (fleetrec.Fleet, error) {
	return fleetrec.At(s.rh.Storer(), branch, s.rh.FactRoot())
}

// FileAt reads the regular file p (a repo-root path) at branch's tip in THIS
// store: ok is false when there is none (a symlink counts as none). A blob
// larger than limit is an error and is not read.
func (s *Service) FileAt(branch, p string, limit int64) ([]byte, bool, error) {
	return fleetrec.FileAt(s.rh.Storer(), branch, p, limit)
}

// FleetKeysOf returns every key agentID's member record has held in this
// store's (a mounted fleet repository's) history of branch: the record's
// VERSIONS, which is where an agent's key history lives. Malformed versions
// are skipped.
func (s *Service) FleetKeysOf(branch, agentID string) ([]ssh.PublicKey, error) {
	return fleetrec.KeysOf(s.rh.Storer(), branch, s.rh.FactRoot(), agentID)
}

// FleetSince is, for each repo path in paths that branch's tip holds, the
// committer time of the oldest commit of branch's first-parent chain from
// which that path's blob is the tip's: when the current version landed on
// main. A path whose current version is older than the maxCommits commits
// walked is left out, and so is one the tip does not hold.
func (s *Service) FleetSince(branch string, paths []string, maxCommits int) (map[string]time.Time, error) {
	return fleetrec.Since(s.rh.Storer(), branch, paths, maxCommits)
}

// FleetMainLog returns the first-parent commits of branch (newest first)
// that add rootCert (the fleet ontology's pki: root path), change the host
// record or a member record under the store's <root>/members/, or merge a
// branch, walking at most maxCommits commits and stopping once maxEntries are
// found: the walk is newest first, so a caller that shows only the newest n
// events needs no more than n entries.
func (s *Service) FleetMainLog(branch, rootCert string, maxCommits, maxEntries int) ([]fleetrec.MainCommit, error) {
	return fleetrec.MainLog(s.rh.Storer(), branch, s.rh.FactRoot(), rootCert, maxCommits, maxEntries)
}
