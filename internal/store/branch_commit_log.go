// Commit-log maintenance methods on repoHandler. These were previously on
// searchIndex but moved here because they only touch repoHandler state
// (gits, repo, db) and are called by multiple subsystems.
package store

import (
	"context"
	"fmt"
	"io"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"

	storegit "knomit/internal/store/git"
)

// populateCommitLog backfills commit_log from the tip of branch.
func (rh *repoHandler) populateCommitLog(ctx context.Context, branch string) error {
	hash, err := rh.resolveRef(ctx, branch)
	if err != nil {
		// Branch not found (empty repo) — just mark available if table exists.
		_ = rh.gits.CommitLogAvailable()
		return nil
	}

	logIter, err := rh.repo.Log(&gogit.LogOptions{
		From:  hash,
		Order: gogit.LogOrderDefault,
	})
	if err != nil {
		return fmt.Errorf("populateCommitLog: log: %w", err)
	}
	defer logIter.Close()

	var count int
	err = rh.gits.CommitLogSync(branch, func() (string, []storegit.CommitLogEntry, error) {
		c, err := logIter.Next()
		if err == io.EOF {
			return "", nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		count++
		files, err := changedFilesInCommit(c)
		if err != nil {
			return "", nil, err
		}
		return c.Hash.String(), commitEntries(c, files), nil
	})
	if err != nil {
		return fmt.Errorf("populateCommitLog: sync: %w", err)
	}

	log.Debug().Int("commits", count).Msg("commit_log: populated")
	return nil
}

// AppendCommitLog inserts a single new commit into commit_log.
func (rh *repoHandler) AppendCommitLog(ctx context.Context, branch, hashStr string) {
	if !rh.gits.CommitLogAvailable() {
		return
	}
	hash := plumbing.NewHash(hashStr)
	c, err := rh.repo.CommitObject(hash)
	if err != nil {
		log.Warn().Err(err).Str("hash", hashStr).Msg("commit_log: get commit")
		return
	}
	files, err := changedFilesInCommit(c)
	if err != nil {
		log.Warn().Err(err).Str("hash", hashStr).Msg("commit_log: changed files")
		return
	}
	done := false
	entries := commitEntries(c, files)
	if err := rh.gits.CommitLogSync(branch, func() (string, []storegit.CommitLogEntry, error) {
		if done {
			return "", nil, nil
		}
		done = true
		return hash.String(), entries, nil
	}); err != nil {
		log.Warn().Err(err).Str("hash", hash.String()).Msg("commit_log: append sync failed")
	}
}
