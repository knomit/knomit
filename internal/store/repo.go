// Repo constructors: OpenRepo, InitRepo, CloneFrom, InitFromRemote.
// These populate the git-related fields on Service (repo, auth, signer, etc.).
package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog/log"

)

// initRepoConfig stamps the repo config with the knomit identity and disables
// GPG signing, which must be done for both fresh and remote-initialized repos.
func initRepoConfig(repo *gogit.Repository, label string) error {
	cfg, err := repo.Config()
	if err != nil {
		return fmt.Errorf("%s: read config: %w", label, err)
	}
	cfg.User.Name = "knomit"
	cfg.User.Email = "knomit@local"
	if cfg.Raw != nil {
		cfg.Raw.Section("commit").SetOption("gpgsign", "false")
	}
	if err := repo.SetConfig(cfg); err != nil {
		return fmt.Errorf("%s: set config: %w", label, err)
	}
	return nil
}

// defaultAgentBranch returns "agent/<hostname>", falling back to "agent/local".
func defaultAgentBranch() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "local"
	}
	return "agent/" + hostname
}

// OpenRepo opens an existing knomit git repo using the Service's storer.
// Populates s.rh.repo and backfills the commit log.
func (s *Service) OpenRepo() error {
	repo, err := gogit.Open(s.rh.gits, memfs.New())
	if err != nil {
		return fmt.Errorf("OpenRepo: git open: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("OpenRepo: read HEAD: %w", err)
	}

	branch := strings.TrimPrefix(head.Name().String(), "refs/heads/")

	log.Info().Str("branch", branch).Msg("git store opened")
	s.rh.repo = repo
	if err := s.si.populateCommitLog(context.Background(), branch); err != nil {
		log.Warn().Err(err).Msg("commit_log: open populate failed")
	}
	return nil
}

// InitRepo creates a new knomit git repo using the Service's storer.
// initFiles are additional files to create in the initial commit.
// agentBranch defaults to "agent/<hostname>" if empty.
func (s *Service) InitRepo(initFiles map[string]string, agentBranch string) error {
	repo, err := gogit.Init(s.rh.gits, memfs.New())
	if err != nil {
		return fmt.Errorf("InitRepo: git init: %w", err)
	}

	if err := initRepoConfig(repo, "InitRepo"); err != nil {
		return err
	}

	rootManifest := "# Knowledge Base\n\nRoot manifest.\n"
	initSig := object.Signature{Name: "knomit", Email: "knomit@local", When: time.Now()}
	lastCommit, _, err := writeFileToStore(s.rh.gits, plumbing.ZeroHash, "kb.md", rootManifest, "init: create knowledge base", initSig, initSig)
	if err != nil {
		return fmt.Errorf("InitRepo: initial commit: %w", err)
	}

	for path, content := range initFiles {
		lastCommit, _, err = writeFileToStore(s.rh.gits, lastCommit, path, content, "init: "+path, initSig, initSig)
		if err != nil {
			return fmt.Errorf("InitRepo: write %s: %w", path, err)
		}
	}

	if agentBranch == "" {
		agentBranch = defaultAgentBranch()
	}
	agentRefName := plumbing.NewBranchReferenceName(agentBranch)

	agentRef := plumbing.NewHashReference(agentRefName, lastCommit)
	if err := s.rh.gits.SetReference(agentRef); err != nil {
		return fmt.Errorf("InitRepo: set agent ref: %w", err)
	}

	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)
	if err := s.rh.gits.SetReference(headRef); err != nil {
		return fmt.Errorf("InitRepo: set HEAD: %w", err)
	}

	mainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), lastCommit)
	if err := s.rh.gits.SetReference(mainRef); err != nil {
		return fmt.Errorf("InitRepo: set main ref: %w", err)
	}

	log.Info().Str("branch", agentBranch).Msg("git store initialized")
	s.rh.repo = repo
	if err := s.si.populateCommitLog(context.Background(), agentBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: initial populate failed")
	}
	return nil
}

// progressWriter adapts a progress callback to io.Writer for use with CloneOptions.Progress.
type progressWriter struct {
	fn func(string)
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	pw.fn(string(p))
	return len(p), nil
}

// CloneFrom clones a remote URL into the Service's storer.
func (s *Service) CloneFrom(url string, auth transport.AuthMethod, progress func(string)) error {
	opts := &gogit.CloneOptions{
		URL:  url,
		Auth: auth,
	}
	if progress != nil {
		opts.Progress = &progressWriter{fn: progress}
	}

	repo, err := gogit.Clone(s.rh.gits, memfs.New(), opts)
	if err != nil {
		return fmt.Errorf("CloneFrom: clone: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("CloneFrom: resolve HEAD: %w", err)
	}
	branch := strings.TrimPrefix(head.Name().String(), "refs/heads/")

	log.Info().Str("branch", branch).Str("url", url).Msg("cloned remote into storer")
	s.rh.repo = repo
	s.fi.auth = auth
	return nil
}

// InitFromRemote initializes a knomit git repo by fetching from a remote origin.
// If the remote has an existing agent branch for this hostname, it is used.
// Otherwise a new agent branch is created from origin/main.
// If the remote is empty (no refs), falls back to creating initial content inline.
func (s *Service) InitFromRemote(originURL string, auth transport.AuthMethod, agentBranch string) error {
	repo, err := gogit.Init(s.rh.gits, memfs.New())
	if err != nil {
		return fmt.Errorf("InitFromRemote: git init: %w", err)
	}

	if err := initRepoConfig(repo, "InitFromRemote"); err != nil {
		return err
	}

	_, err = repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{originURL},
		Fetch: []gogitconfig.RefSpec{
			"+refs/heads/*:refs/remotes/origin/*",
		},
	})
	if err != nil {
		return fmt.Errorf("InitFromRemote: create remote: %w", err)
	}

	err = repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
	})
	if err == transport.ErrEmptyRemoteRepository {
		return s.initFromEmptyRemote(repo, originURL, auth, agentBranch)
	}
	if err != nil {
		return fmt.Errorf("InitFromRemote: fetch: %w", err)
	}

	if agentBranch == "" {
		agentBranch = defaultAgentBranch()
	}
	agentRefName := plumbing.NewBranchReferenceName(agentBranch)

	// Check for existing remote agent branch.
	remoteAgentRef, err := s.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", agentBranch))
	if err == nil {
		// Remote agent branch exists — use it.
		localRef := plumbing.NewHashReference(agentRefName, remoteAgentRef.Hash())
		if err := s.rh.gits.SetReference(localRef); err != nil {
			return fmt.Errorf("InitFromRemote: set agent ref: %w", err)
		}
	} else {
		// No remote agent branch — create from origin/main.
		originMainRef, err := s.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", "main"))
		if err != nil {
			return fmt.Errorf("InitFromRemote: resolve origin/main: %w", err)
		}
		localRef := plumbing.NewHashReference(agentRefName, originMainRef.Hash())
		if err := s.rh.gits.SetReference(localRef); err != nil {
			return fmt.Errorf("InitFromRemote: set agent ref from main: %w", err)
		}
	}

	// Set HEAD to agent branch.
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)
	if err := s.rh.gits.SetReference(headRef); err != nil {
		return fmt.Errorf("InitFromRemote: set HEAD: %w", err)
	}

	// Create local main pointing at origin/main.
	originMainRef, err := s.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", "main"))
	if err != nil {
		return fmt.Errorf("InitFromRemote: resolve origin/main for local main: %w", err)
	}
	mainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), originMainRef.Hash())
	if err := s.rh.gits.SetReference(mainRef); err != nil {
		return fmt.Errorf("InitFromRemote: set main ref: %w", err)
	}

	log.Info().Str("branch", agentBranch).Str("origin", originURL).Msg("git store initialized from remote")
	s.rh.repo = repo
	s.fi.auth = auth
	if err := s.si.populateCommitLog(context.Background(), agentBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: remote populate failed")
	}
	return nil
}

// initFromEmptyRemote handles the empty-remote fallback for InitFromRemote.
func (s *Service) initFromEmptyRemote(repo *gogit.Repository, originURL string, auth transport.AuthMethod, agentBranch string) error {
	rootManifest := "# Knowledge Base\n\nRoot manifest.\n"
	initSig := object.Signature{Name: "knomit", Email: "knomit@local", When: time.Now()}
	lastCommit, _, writeErr := writeFileToStore(s.rh.gits, plumbing.ZeroHash, "kb.md", rootManifest, "init: create knowledge base", initSig, initSig)
	if writeErr != nil {
		return fmt.Errorf("InitFromRemote: empty remote fallback: %w", writeErr)
	}
	if agentBranch == "" {
		agentBranch = defaultAgentBranch()
	}
	agentRefName := plumbing.NewBranchReferenceName(agentBranch)
	if writeErr = s.rh.gits.SetReference(plumbing.NewHashReference(agentRefName, lastCommit)); writeErr != nil {
		return fmt.Errorf("InitFromRemote: empty remote set agent ref: %w", writeErr)
	}
	if writeErr = s.rh.gits.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)); writeErr != nil {
		return fmt.Errorf("InitFromRemote: empty remote set HEAD: %w", writeErr)
	}
	if writeErr = s.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), lastCommit)); writeErr != nil {
		return fmt.Errorf("InitFromRemote: empty remote set main: %w", writeErr)
	}
	log.Info().Str("branch", agentBranch).Str("origin", originURL).Msg("git store initialized (empty remote)")
	s.rh.repo = repo
	s.fi.auth = auth
	if err := s.si.populateCommitLog(context.Background(), agentBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: empty-remote populate failed")
	}
	return nil
}

