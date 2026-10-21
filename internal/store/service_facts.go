package store

import (
	"context"
)

// TestingSetRef moves (or creates) a git reference WITHOUT a commit and without
// NotifyCommit, for tests in sibling packages. See vcs.Repo.SetRefForTest.
func (s *Service) TestingSetRef(name, hash string) error { return s.rh.SetRefForTest(name, hash) }

// TestingDeleteRef removes a git reference WITHOUT NotifyCommit. See
// vcs.Repo.DeleteRefForTest.
func (s *Service) TestingDeleteRef(name string) error { return s.rh.DeleteRefForTest(name) }

// TestingCommitFiles commits files onto branch as ONE signed commit, skipping
// NotifyCommit. See vcs.Repo.CommitFilesForTest.
func (s *Service) TestingCommitFiles(branch string, files map[string]string, message string) (string, error) {
	return s.rh.CommitFilesForTest(branch, files, message)
}

// Triggers returns the trigger dispatcher's store surface.
func (s *Service) Triggers() TriggerIndex { return s.rh }

// Templates returns the template reader of this store.
func (s *Service) Templates() TemplateIndex { return s.rh }

// WriteSystemTree writes files as ONE authored commit on branch, keeping the
// case of every path. See facts.Index.WriteSystemTree.
func (s *Service) WriteSystemTree(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error) {
	return s.fi.WriteSystemTree(ctx, branch, files, message, operation)
}

// SystemFiles returns the system-file reader of this store.
func (s *Service) SystemFiles() SystemFileIndex { return s.rh }

// Skills returns the skill reader of this store.
func (s *Service) Skills() SkillIndex { return s.rh }

// SignerFingerprint is the full pki.Fingerprint of the key this store signs
// authored commits with, or "" when it has none. See repoHandler.signerFingerprint.
func (s *Service) SignerFingerprint() string { return s.rh.SignerFingerprint() }
