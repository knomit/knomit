// Repo constructors: OpenRepo and InitRepo (local creation). The remote
// bootstrap (CloneFrom, InitFromRemote, InitSubscription) is on
// remotesync.Index (internal/store/remotesync/bootstrap.go), reached as
// svc.Remote().
package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/remotesync"
)

// OpenRepo opens an existing knomit git repo using the Service's storer.
// Populates s.rh.repo and backfills the commit log.
func (s *Service) OpenRepo() error {
	repo, err := gogit.Open(s.rh.Storer(), memfs.New())
	if err != nil {
		return fmt.Errorf("OpenRepo: git open: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("OpenRepo: read HEAD: %w", err)
	}

	branch := strings.TrimPrefix(head.Name().String(), "refs/heads/")

	log.Info().Str("branch", branch).Msg("git store opened")
	s.rh.SetRepository(repo)
	if _, err := s.rh.EnsureBranch(context.Background(), branch, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("OpenRepo: ensure branch %q: %w", branch, err)
	}
	// Backfill commit_parents for commits indexed before that table existed.
	// Order no longer matters for history derivation (it reads parents from
	// git objects); it is kept first so RevisionsBefore and the graph
	// resolvers see a complete parent graph as early as possible.
	if err := vcs.BackfillCommitParents(context.Background(), s.rh.Repo); err != nil {
		log.Warn().Err(err).Msg("commit_parents: backfill failed")
	}
	// History is derived before the repo serves anything (OpenHistory); a
	// failure keeps it closed, and the next open retries.
	if err := s.rh.OpenHistory(context.Background()); err != nil {
		return fmt.Errorf("OpenRepo: history: %w", err)
	}
	if err := s.rh.PopulateCommitLog(context.Background(), branch); err != nil {
		log.Warn().Err(err).Msg("commit_log: open populate failed")
	}
	return nil
}

// InitRepo creates a new knomit git repo using the Service's storer, with
// DefaultConsensusBranch as the local consensus branch name (nobody named one,
// and a local create has nothing to detect it from). The name is recorded, so
// it is read back rather than assumed from then on. initFiles are additional
// files to create in the initial commit. agentBranch defaults to
// "agent/<hostname>" if empty. Thin wrapper around InitRepoWithUpstream.
func (s *Service) InitRepo(ctx context.Context, initFiles map[string]string, agentBranch string) error {
	return s.InitRepoWithUpstream(ctx, initFiles, "", agentBranch)
}

// InitRepoWithUpstream is InitRepo parameterized on the local consensus
// branch name. An empty name is a create nobody named a branch for, and gets
// DefaultConsensusBranch; production callers otherwise go through
// InitFromRemote, which takes the name from the remote.
func (s *Service) InitRepoWithUpstream(ctx context.Context, initFiles map[string]string, upstreamMain, agentBranch string) error {
	if upstreamMain == "" {
		upstreamMain = remotesync.DefaultConsensusBranch
	}
	repo, err := gogit.Init(s.rh.Storer(), memfs.New())
	if err != nil {
		return fmt.Errorf("InitRepo: git init: %w", err)
	}

	if err := vcs.InitRepoConfig(repo, "InitRepo"); err != nil {
		return err
	}

	rootManifest := "# Knowledge Base\n\nRoot manifest.\n"
	initSig := object.Signature{Name: "knomit", Email: "knomit@local", When: time.Now()}
	// The nonce guarantees distinct root-commit hashes for repos created in
	// the same second (git timestamps have second precision and everything
	// else in this commit is fixed) — the root hash is the repo's stable
	// identity (lenses RFC decision 11).
	//
	// This local init has no origin, so distinct roots for independent inits are
	// exactly the intent (decision 11). The user-visible hazard is the shared
	// EMPTY-remote case in initFromEmptyRemote — see the ACCEPTED RACE note there
	// for why two machines can silently diverge and why a real fix needs
	// init-time cross-machine coordination rather than weakening the nonce.
	initMsg := fmt.Sprintf("init: create knowledge base\n\nknomit-repo-nonce: %s", uuid.New().String())
	lastCommit, _, err := vcs.WriteFileToStore(s.rh.Storer(), nil, plumbing.ZeroHash, "README.md", rootManifest, initMsg, initSig, initSig)
	if err != nil {
		return fmt.Errorf("InitRepo: initial commit: %w", err)
	}

	for path, content := range initFiles {
		lastCommit, _, err = vcs.WriteFileToStore(s.rh.Storer(), nil, lastCommit, path, content, "init: "+path, initSig, initSig)
		if err != nil {
			return fmt.Errorf("InitRepo: write %s: %w", path, err)
		}
	}

	if agentBranch == "" {
		agentBranch = vcs.DefaultAgentBranch()
	}
	agentRefName := plumbing.NewBranchReferenceName(agentBranch)

	agentRef := plumbing.NewHashReference(agentRefName, lastCommit)
	if err := s.rh.Storer().SetReference(agentRef); err != nil {
		return fmt.Errorf("InitRepo: set agent ref: %w", err)
	}

	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)
	if err := s.rh.Storer().SetReference(headRef); err != nil {
		return fmt.Errorf("InitRepo: set HEAD: %w", err)
	}

	mainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(upstreamMain), lastCommit)
	if err := s.rh.Storer().SetReference(mainRef); err != nil {
		return fmt.Errorf("InitRepo: set %s ref: %w", upstreamMain, err)
	}
	// Local init records the consensus branch it created: with no origin,
	// UpstreamBranch answers this name (remotesync/consensus_branch.go).
	s.ri.RecordConsensusBranch(upstreamMain)

	// Seed the per-agent watermark to the initial commit (which is exactly
	// the local-upstream hash at this point). On the next reconcileAgent the
	// walk will use this as its stop point.
	if err := s.rh.WriteAgentBase(agentBranch, lastCommit); err != nil {
		return fmt.Errorf("InitRepo: seed agent watermark: %w", err)
	}

	log.Info().Str("branch", agentBranch).Str("upstream", upstreamMain).Msg("git store initialized")
	s.rh.SetRepository(repo)
	if _, err := s.rh.EnsureBranch(ctx, agentBranch, "refs/heads/"+agentBranch); err != nil {
		return fmt.Errorf("InitRepo: ensure agent branch %q: %w", agentBranch, err)
	}
	if _, err := s.rh.EnsureBranch(ctx, upstreamMain, "refs/heads/"+upstreamMain); err != nil {
		return fmt.Errorf("InitRepo: ensure upstream branch %q: %w", upstreamMain, err)
	}
	// History is derived before the repo serves anything (OpenHistory); a
	// failure keeps it closed, and the next open retries.
	if err := s.rh.OpenHistory(ctx); err != nil {
		return fmt.Errorf("InitRepo: history: %w", err)
	}
	if err := s.rh.PopulateCommitLog(ctx, agentBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: initial populate failed")
	}
	if err := s.rh.PopulateCommitLog(ctx, upstreamMain); err != nil {
		log.Warn().Err(err).Str("branch", upstreamMain).Msg("commit_log: initial populate (upstream) failed")
	}
	return nil
}
