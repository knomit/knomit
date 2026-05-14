// Repo constructors: OpenRepo, InitRepo, CloneFrom, InitFromRemote.
// These populate the git-related fields on Service (repo, auth, signer, etc.).
package store

import (
	"context"
	"errors"
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
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/rs/zerolog/log"
)

// ErrEmptyRemote is returned by CloneFrom when the remote repository exists
// but contains no branches/commits. Knomit's sync model requires the remote
// to have at least one branch (typically "main") to merge into the agent
// branch, so an empty remote cannot be connected.
var ErrEmptyRemote = errors.New("remote repository has no branches")

// ErrBranchNotFound is returned by ref-resolving code paths (ListDir, ReadFact,
// HeadCommit, etc.) when the requested branch has no matching git ref. Handlers
// in internal/web detect this via errors.Is and map it to HTTP 404 instead of
// 500. The underlying go-git plumbing.ErrReferenceNotFound is preserved in the
// error chain for introspection.
var ErrBranchNotFound = errors.New("branch not found")

// ErrFactNotLive is returned by ExplainFact (and any other HEAD-anchored read
// path that needs to resolve a path's active commit via branch_facts) when
// no row exists for (branch, path). This means the fact is not currently live
// at HEAD — typically because it was retracted, or because the path was never
// indexed on this branch. Older versions may still exist in the graph and be
// reachable via commit-anchored endpoints.
var ErrFactNotLive = errors.New("fact not live at HEAD")

// ErrPathNotFound is returned by ReadFact (and the underlying readFile* helpers)
// when the requested path does not exist at the requested anchor. This is the
// "true 404" condition — distinct from a git/database error reading a path that
// should exist. Callers use errors.Is to surface a 404 while propagating real
// errors as 500.
var ErrPathNotFound = errors.New("path not found")

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
	if _, err := s.rh.EnsureBranch(context.Background(), branch, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("OpenRepo: ensure branch %q: %w", branch, err)
	}
	if err := s.rh.populateCommitLog(context.Background(), branch); err != nil {
		log.Warn().Err(err).Msg("commit_log: open populate failed")
	}
	return nil
}

// InitRepo creates a new knomit git repo using the Service's storer with
// "main" as the local consensus branch name. initFiles are additional files
// to create in the initial commit. agentBranch defaults to "agent/<hostname>"
// if empty. Thin wrapper around InitRepoWithUpstream — most callers (no
// origin, default branch naming) should use this.
func (s *Service) InitRepo(initFiles map[string]string, agentBranch string) error {
	return s.InitRepoWithUpstream(initFiles, "main", agentBranch)
}

// InitRepoWithUpstream is InitRepo parameterized on the local consensus
// branch name. Use when the local repo should default to "master" (or any
// non-"main" name) — typically only test fixtures need this; production
// callers either go through InitFromRemote (which detects from the remote)
// or use the default "main".
func (s *Service) InitRepoWithUpstream(initFiles map[string]string, upstreamMain, agentBranch string) error {
	if upstreamMain == "" {
		upstreamMain = "main"
	}
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

	mainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(upstreamMain), lastCommit)
	if err := s.rh.gits.SetReference(mainRef); err != nil {
		return fmt.Errorf("InitRepo: set %s ref: %w", upstreamMain, err)
	}

	// Seed the per-agent watermark to the initial commit (which is exactly
	// the local-upstream hash at this point). On the next reconcileAgent the
	// walk will use this as its stop point.
	if err := s.rh.writeAgentBase(agentBranch, lastCommit); err != nil {
		return fmt.Errorf("InitRepo: seed agent watermark: %w", err)
	}

	log.Info().Str("branch", agentBranch).Str("upstream", upstreamMain).Msg("git store initialized")
	s.rh.repo = repo
	if _, err := s.rh.EnsureBranch(context.Background(), agentBranch, "refs/heads/"+agentBranch); err != nil {
		return fmt.Errorf("InitRepo: ensure agent branch %q: %w", agentBranch, err)
	}
	if _, err := s.rh.EnsureBranch(context.Background(), upstreamMain, "refs/heads/"+upstreamMain); err != nil {
		return fmt.Errorf("InitRepo: ensure upstream branch %q: %w", upstreamMain, err)
	}
	if err := s.rh.populateCommitLog(context.Background(), agentBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: initial populate failed")
	}
	if err := s.rh.populateCommitLog(context.Background(), upstreamMain); err != nil {
		log.Warn().Err(err).Str("branch", upstreamMain).Msg("commit_log: initial populate (upstream) failed")
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
// Returns ErrEmptyRemote when the remote exists but has no branches yet —
// knomit's sync model requires at least one branch on the remote.
func (s *Service) CloneFrom(url string, auth transport.AuthMethod, progress func(string)) error {
	opts := &gogit.CloneOptions{
		URL:  url,
		Auth: auth,
	}
	if progress != nil {
		opts.Progress = &progressWriter{fn: progress}
	}

	repo, cloneErr := gogit.Clone(s.rh.gits, memfs.New(), opts)
	if errors.Is(cloneErr, transport.ErrEmptyRemoteRepository) {
		return ErrEmptyRemote
	}
	if cloneErr != nil {
		return fmt.Errorf("CloneFrom: clone: %w", cloneErr)
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
//
// upstreamMain selects the remote's consensus branch (typically "main",
// configurable to "master" or any other name). When empty, the helper
// inspects the remote's symbolic HEAD after the initial fetch and uses
// whatever it points at, falling back to "main" if detection fails.
//
// If the remote has an existing agent branch for this hostname, it is used.
// Otherwise a new agent branch is created from origin/<upstreamMain>.
// If the remote is empty (no refs), falls back to creating initial content
// inline — initFiles are written as seed files on the new agent branch in
// that case, and are ignored when the remote already has branches.
func (s *Service) InitFromRemote(originURL string, auth transport.AuthMethod, upstreamMain, agentBranch string, initFiles map[string]string) error {
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
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return s.initFromEmptyRemote(repo, originURL, auth, upstreamMain, agentBranch, initFiles)
	}
	if err != nil {
		return fmt.Errorf("InitFromRemote: fetch: %w", err)
	}

	if agentBranch == "" {
		agentBranch = defaultAgentBranch()
	}

	// Detect the remote's default branch when the caller didn't specify.
	// Falls back to "main" if the remote has no symbolic HEAD we can read.
	if upstreamMain == "" {
		upstreamMain = detectRemoteUpstream(repo, auth)
		if upstreamMain == "" {
			log.Warn().Msg("InitFromRemote: could not detect remote HEAD; defaulting to \"main\"")
			upstreamMain = "main"
		} else {
			log.Info().Str("upstream", upstreamMain).Msg("InitFromRemote: detected upstream branch from remote HEAD")
		}
	}

	// Publish the freshly-initialised repo on the handler BEFORE
	// configureRemote — that helper reaches into rh.repo to read and rewrite
	// the git config, and would panic on nil otherwise. The rest of
	// InitFromRemote also depends on rh.repo being live for the remote-
	// tracking ref lookups below.
	s.rh.repo = repo

	// Configure remote with two-refspec fetch (main + agent/<host>).
	// The initial CreateRemote above used a wildcard refspec to discover all
	// remote branches at bootstrap; now we lock it down for steady state.
	if err := s.rh.configureRemote(originURL, upstreamMain, agentBranch); err != nil {
		return fmt.Errorf("InitFromRemote: configure remote: %w", err)
	}

	// Re-fetch with the proper refspec so origin/<upstreamMain> and
	// origin/agent/<host> are both tracked consistently. (The initial
	// wildcard fetch already pulled objects; this just establishes the
	// remote-tracking refs under the new refspec shape.) Use fetchOrigin so
	// the agent ref's absence on origin (typical first connect) is tolerated.
	if err := fetchOrigin(repo, auth, upstreamMain); err != nil {
		return fmt.Errorf("InitFromRemote: re-fetch: %w", err)
	}

	// Bootstrap local upstream branch from origin/<upstreamMain>.
	originMainRef, err := s.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", upstreamMain))
	if err != nil {
		return fmt.Errorf("InitFromRemote: resolve origin/%s: %w", upstreamMain, err)
	}
	if err := s.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(upstreamMain), originMainRef.Hash())); err != nil {
		return fmt.Errorf("InitFromRemote: set local %s: %w", upstreamMain, err)
	}

	// Bootstrap local agent and compute the watermark in the same branch
	// so the origin/agent lookup happens exactly once.
	//
	// If origin/agent/<host> exists, adopt it: the local agent ref points
	// at the adopted tip, and the watermark must be the main commit that
	// chain last branched from, i.e. MergeBase(origin/agent, origin/main).
	// Seeding the watermark to current origin/main would be wrong when
	// origin/main has advanced past the last push — the current main
	// commit is not on origin/agent's first-parent chain, so the next
	// reconcile's unpushedCommits walk would reach root and replay every
	// ancestor (including old main commits), resurrecting files that
	// main has since deleted under StrategyLocalWins.
	//
	// If origin/agent/<host> does NOT exist, bootstrap from origin/main:
	// the agent ref IS origin/main, so the watermark equals it.
	//
	// Watermark = the main commit the agent's chain last branched from.
	// For bootstrap-from-main this equals origin/main. For adoption of an
	// existing origin/agent, this equals MergeBase(origin/agent, origin/main).
	// The watermark commit must be on agent's first-parent chain so the
	// unpushedCommits walk stops there cleanly.
	agentRefName := plumbing.NewBranchReferenceName(agentBranch)
	var watermarkHash plumbing.Hash
	if remoteAgentRef, err := s.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", agentBranch)); err == nil {
		// Adopt path: agent ref points at the adopted origin/agent tip.
		if err := s.rh.gits.SetReference(plumbing.NewHashReference(agentRefName, remoteAgentRef.Hash())); err != nil {
			return fmt.Errorf("InitFromRemote: set agent from remote agent: %w", err)
		}
		remoteAgentCommit, err := s.rh.repo.CommitObject(remoteAgentRef.Hash())
		if err != nil {
			return fmt.Errorf("InitFromRemote: load remote agent commit: %w", err)
		}
		originMainCommit, err := s.rh.repo.CommitObject(originMainRef.Hash())
		if err != nil {
			return fmt.Errorf("InitFromRemote: load origin main commit: %w", err)
		}
		bases, err := remoteAgentCommit.MergeBase(originMainCommit)
		if err != nil {
			return fmt.Errorf("InitFromRemote: merge-base(remote agent, origin main): %w", err)
		}
		if len(bases) == 0 {
			// Disjoint histories — fall back to current origin/main and
			// let the next reconcile's disjoint-replay handle it. Shape
			// matches scenario G2.
			watermarkHash = originMainRef.Hash()
		} else {
			watermarkHash = bases[0].Hash
		}
	} else {
		// Bootstrap-from-main path: agent ref is origin/main, watermark
		// equals it.
		if err := s.rh.gits.SetReference(plumbing.NewHashReference(agentRefName, originMainRef.Hash())); err != nil {
			return fmt.Errorf("InitFromRemote: set agent from main: %w", err)
		}
		watermarkHash = originMainRef.Hash()
	}

	if err := s.rh.gits.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)); err != nil {
		return fmt.Errorf("InitFromRemote: set HEAD: %w", err)
	}

	if err := s.rh.writeAgentBase(agentBranch, watermarkHash); err != nil {
		return fmt.Errorf("InitFromRemote: seed agent watermark: %w", err)
	}

	log.Info().Str("branch", agentBranch).Str("upstream", upstreamMain).Str("origin", originURL).Msg("git store initialized from remote")
	// s.rh.repo is already published (set earlier so configureRemote could run).
	s.fi.auth = auth
	if _, err := s.rh.EnsureBranch(context.Background(), agentBranch, "refs/heads/"+agentBranch); err != nil {
		return fmt.Errorf("InitFromRemote: ensure agent branch %q: %w", agentBranch, err)
	}
	if _, err := s.rh.EnsureBranch(context.Background(), upstreamMain, "refs/heads/"+upstreamMain); err != nil {
		return fmt.Errorf("InitFromRemote: ensure upstream branch %q: %w", upstreamMain, err)
	}
	if err := s.rh.populateCommitLog(context.Background(), agentBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: remote populate failed")
	}
	if err := s.rh.populateCommitLog(context.Background(), upstreamMain); err != nil {
		log.Warn().Err(err).Str("branch", upstreamMain).Msg("commit_log: remote populate (upstream) failed")
	}
	return nil
}

// detectRemoteUpstream queries origin's symbolic HEAD to determine the default
// branch (e.g. "main", "master"). Returns "" when detection fails; the caller
// must fall back to "main".
//
// Uses remote.List (which issues a separate ls-remote round-trip) rather than
// reading a local ref, because go-git's default fetch refspecs do not bring
// HEAD into refs/remotes/origin/. The extra round-trip happens at repo init
// only.
func detectRemoteUpstream(repo *gogit.Repository, auth transport.AuthMethod) string {
	remote, err := repo.Remote("origin")
	if err != nil {
		return ""
	}
	return detectFromRemote(remote, auth)
}

// DetectRemoteUpstreamFromURL is the public detection entry point used by
// the repos builder at first-boot — before any local repo exists to attach
// a remote to. Creates a throwaway in-memory repo, attaches `origin` to the
// given URL, and queries its symbolic HEAD. Returns "" when the remote
// cannot be reached or has no symbolic HEAD; the caller must fall back to
// "main".
func DetectRemoteUpstreamFromURL(url string, auth transport.AuthMethod) string {
	storage := memory.NewStorage()
	repo, err := gogit.Init(storage, nil)
	if err != nil {
		return ""
	}
	remote, err := repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{url},
	})
	if err != nil {
		return ""
	}
	return detectFromRemote(remote, auth)
}

func detectFromRemote(remote *gogit.Remote, auth transport.AuthMethod) string {
	refs, err := remote.List(&gogit.ListOptions{Auth: auth})
	if err != nil {
		return ""
	}
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference {
			return strings.TrimPrefix(ref.Target().String(), "refs/heads/")
		}
	}
	return ""
}

// initFromEmptyRemote handles the empty-remote fallback for InitFromRemote.
// initFiles are written as additional seed files on top of the root manifest
// so the new agent branch matches the layout produced by InitRepo.
// upstreamMain is the consensus branch name to bootstrap (defaults to "main"
// when empty). The empty-remote path has no remote HEAD to detect from, so
// the caller's value (or "main") is authoritative.
func (s *Service) initFromEmptyRemote(repo *gogit.Repository, originURL string, auth transport.AuthMethod, upstreamMain, agentBranch string, initFiles map[string]string) error {
	if upstreamMain == "" {
		upstreamMain = "main"
	}
	rootManifest := "# Knowledge Base\n\nRoot manifest.\n"
	initSig := object.Signature{Name: "knomit", Email: "knomit@local", When: time.Now()}
	lastCommit, _, writeErr := writeFileToStore(s.rh.gits, plumbing.ZeroHash, "kb.md", rootManifest, "init: create knowledge base", initSig, initSig)
	if writeErr != nil {
		return fmt.Errorf("InitFromRemote: empty remote fallback: %w", writeErr)
	}
	for path, content := range initFiles {
		lastCommit, _, writeErr = writeFileToStore(s.rh.gits, lastCommit, path, content, "init: "+path, initSig, initSig)
		if writeErr != nil {
			return fmt.Errorf("InitFromRemote: empty remote write %s: %w", path, writeErr)
		}
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
	if writeErr = s.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(upstreamMain), lastCommit)); writeErr != nil {
		return fmt.Errorf("InitFromRemote: empty remote set %s: %w", upstreamMain, writeErr)
	}
	// Seed the watermark — same shape as the non-empty remote bootstrap.
	if writeErr = s.rh.writeAgentBase(agentBranch, lastCommit); writeErr != nil {
		return fmt.Errorf("InitFromRemote: empty remote seed agent watermark: %w", writeErr)
	}
	log.Info().Str("branch", agentBranch).Str("origin", originURL).Msg("git store initialized (empty remote)")
	s.rh.repo = repo
	s.fi.auth = auth
	if _, err := s.rh.EnsureBranch(context.Background(), agentBranch, "refs/heads/"+agentBranch); err != nil {
		return fmt.Errorf("InitFromRemote: empty remote ensure agent branch %q: %w", agentBranch, err)
	}
	if _, err := s.rh.EnsureBranch(context.Background(), upstreamMain, "refs/heads/"+upstreamMain); err != nil {
		return fmt.Errorf("InitFromRemote: empty remote ensure upstream branch %q: %w", upstreamMain, err)
	}
	if err := s.rh.populateCommitLog(context.Background(), agentBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: empty-remote populate failed")
	}
	if err := s.rh.populateCommitLog(context.Background(), upstreamMain); err != nil {
		log.Warn().Err(err).Str("branch", upstreamMain).Msg("commit_log: empty-remote populate (upstream) failed")
	}
	return nil
}

