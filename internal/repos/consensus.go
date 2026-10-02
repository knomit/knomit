// The consensus merger (F08, ruling R5): with `consensus: auto` in the root
// attributes of the ontology at the tip of the repo's consensus branch, the
// instance that OWNS that branch — the one whose repo has no origin — merges
// every branch a peer pushes to it (F11 receive-pack) into its OWN agent
// branch, as soon as the push lands. The consensus branch then follows the
// agent branch through the ordinary local reconcile, which is why the merge
// never targets the consensus branch itself: a merge there would stop it being
// an ancestor of the agent branch, and the local reconcile only fast-forwards
// (kb/invariants/store/local-reconcile/never-reset-never-exit), so the host's
// own facts would never reach it again.
//
// Shape, like the trigger dispatcher: ri.onCommit (under the writer's branch
// lock) only schedules — one non-blocking send on a 1-slot channel — and the
// merge runs on this goroutine, so a push's POST never waits for it. It is
// kicked by a commit on any branch that is not the agent branch and not an
// experiment: a pushed peer branch (receive-pack's register), and the
// consensus branch advancing (N2: turning `consensus: auto` on acts at once,
// without waiting for the next push). It also runs once at start.
//
// Each merge is Service.MergeConsensus: a host-signed merge commit, conflicts
// REFUSED with no side taken. A refused (branch, tip) is remembered, warned
// once, and not retried until that branch's tip moves; the branch stays in
// the Pushed branches list for a human's UI merge. After any merge the sync
// loop is woken, so the consensus branch follows within its countdown.
//
// Convergence (T-B4): the store no-ops a peer tip already in the agent branch
// (a peer that fast-forwarded to the host's merge), and MergeConsensus also
// no-ops a tip that only merged the host back in and changes nothing (a peer
// whose sync always writes a merge commit). Either way two idle instances
// stop writing commits.
package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/platform/crashdump"
	"knomit/internal/store"
)

// consensusHooks are test seams; every field is nil in production.
type consensusHooks struct {
	// beforeMerge runs right before MergeConsensus for one pushed branch: a
	// test parks the merger here to prove the push completed without it.
	beforeMerge func(branch string)
}

var (
	consensusHooksMu   sync.Mutex
	consensusTestHooks consensusHooks
)

func currentConsensusHooks() consensusHooks {
	consensusHooksMu.Lock()
	defer consensusHooksMu.Unlock()
	return consensusTestHooks
}

// consensusStats is what the merger has done since start; tests read it.
type consensusStats struct {
	Runs     int      // completed runs, whatever they decided
	Attempts int      // MergeConsensus calls (a refused tip not retried is not one)
	Merges   int      // merge commits written
	Refused  int      // merges refused (conflict, unrelated histories, no signer)
	Warnings []string // every WARN it logged, in order (each logged once)
}

// consensusMerger is one repo's merger.
type consensusMerger struct {
	ri          *RepoInstance
	repo        string
	agentBranch string
	kick        chan struct{}

	// life guards cancel/started/stopped: start is idempotent and a no-op
	// once stop has run, because SwapStore may start the merger after the
	// heal that would have started it was cancelled, concurrently with a
	// teardown (see lifetimeGuard).
	life lifetimeGuard
	wg   sync.WaitGroup

	mu sync.Mutex
	// refused maps a pushed branch to the tip whose merge was refused: that
	// tip is not tried again, a new one is.
	refused map[string]plumbing.Hash
	// warned holds the keys of every WARN already logged.
	warned map[string]bool
	stats  consensusStats
}

func newConsensusMerger(ri *RepoInstance, repo, agentBranch string) *consensusMerger {
	return &consensusMerger{
		ri:          ri,
		repo:        repo,
		agentBranch: agentBranch,
		kick:        make(chan struct{}, 1),
		refused:     map[string]plumbing.Hash{},
		warned:      map[string]bool{},
	}
}

// start launches the merger on its own context derived from parent (never
// the sync loop's, which ActivateSync restarts) and kicks it once, so pushes
// that landed while the server was down are merged on restart. A second call,
// or a call after stop, is a no-op.
func (m *consensusMerger) start(parent context.Context) {
	ctx, ok := m.life.begin(parent, &m.wg)
	if !ok {
		return
	}
	m.kickNow()
	go m.loop(ctx)
}

// stop cancels the merger and waits for a running merge to return. Safe when
// start never ran; a later start is then a no-op.
func (m *consensusMerger) stop() {
	m.life.end()
	m.wg.Wait()
}

// kickNow is ri.onCommit's whole job for the merger: O(1), never blocks.
func (m *consensusMerger) kickNow() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// consensusKick on the instance; nil-safe (no merger on this repo).
func (ri *RepoInstance) consensusKick() {
	if m := ri.consensus; m != nil {
		m.kickNow()
	}
}

// consensusKicks reports whether a commit on branch kicks the merger: every
// branch except this instance's agent branch and the experiments. That covers
// the pushed branches and the consensus branch (N2) without reading the store,
// which ri.onCommit may not do: it runs under the writer's branch lock.
func consensusKicks(branch, agentBranch string) bool {
	return branch != agentBranch && !strings.HasPrefix(branch, "exp/")
}

func (m *consensusMerger) loop(ctx context.Context) {
	defer m.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.kick:
			m.safeRun(ctx)
		}
	}
}

func (m *consensusMerger) safeRun(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			crashdump.ReportRecovered("consensus:"+m.repo, r)
			log.Error().Str("repo", m.repo).Interface("panic", r).
				Msg("consensus: run panicked; the next kick retries")
		}
	}()
	m.run(ctx)
}

// warnOnce logs msg at WARN the first time key is seen, and records it.
func (m *consensusMerger) warnOnce(key, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.warned[key] {
		return
	}
	m.warned[key] = true
	m.stats.Warnings = append(m.stats.Warnings, msg)
	log.Warn().Str("repo", m.repo).Msg(msg)
}

func (m *consensusMerger) snapshot() consensusStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stats
	s.Warnings = append([]string(nil), m.stats.Warnings...)
	return s
}

func blobKey(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:8])
}

// run is one pass: read the setting at the consensus branch's tip, then try
// every pushed branch.
func (m *consensusMerger) run(ctx context.Context) {
	defer func() {
		m.mu.Lock()
		m.stats.Runs++
		m.mu.Unlock()
	}()
	svc, release, err := m.ri.Acquire()
	if err != nil {
		return // the store is going away or being swapped; the next kick retries
	}
	defer release()

	upstream := svc.UpstreamBranch()
	if !m.autoAt(ctx, svc, upstream) {
		return
	}
	origin, err := svc.Remote().GetRemote("origin")
	if err != nil {
		m.warnOnce("origin-read:"+err.Error(), fmt.Sprintf(
			"consensus: auto: could not read this repo's origin (%v); nothing merged, the next kick retries", err))
		return
	}
	if origin != nil {
		m.warnOnce("origin:"+origin.URL, fmt.Sprintf(
			"consensus: auto ignored: this repo has an origin (%s); the origin owns %s", origin.URL, upstream))
		return
	}

	branches, err := svc.Branches().ListBranches(ctx)
	if err != nil {
		m.warnOnce("list:"+err.Error(), fmt.Sprintf("consensus: list branches: %v; the next kick retries", err))
		return
	}
	merged := false
	for _, b := range branches {
		if ctx.Err() != nil {
			return
		}
		if !m.ri.isPushedBranch(b.Name, upstream) {
			continue
		}
		if m.mergeOne(ctx, svc, b.Name) {
			merged = true
		}
	}
	if merged {
		// The consensus branch follows the agent branch on the sync loop's
		// next round; wake it rather than wait an interval.
		m.ri.wakeSync()
	}
}

// autoAt reports whether the ontology at upstream's tip says consensus: auto.
// Anything else — absent, off, a value this binary does not know, an
// unreadable file — is off; the last two are warned once per ontology blob.
func (m *consensusMerger) autoAt(ctx context.Context, svc *store.Service, upstream string) bool {
	data, err := svc.OntologyAt(ctx, upstream)
	if err != nil {
		if errors.Is(err, store.ErrBranchNotFound) {
			return false // no consensus branch yet: nothing is on
		}
		m.warnOnce("ontology:"+err.Error(), fmt.Sprintf("consensus: read the ontology at %s: %v; read as off", upstream, err))
		return false
	}
	if data == nil {
		return false
	}
	cs, err := fact.ReadConsensus(data)
	if err != nil {
		m.warnOnce("ontology:"+blobKey(data), fmt.Sprintf("consensus: the ontology at %s is unreadable (%v); read as off", upstream, err))
		return false
	}
	if !cs.Valid {
		m.warnOnce("value:"+blobKey(data), fmt.Sprintf(
			"consensus: unknown value %v at %s (this knomit knows \"off\" and \"auto\"); read as off", cs.Raw, upstream))
		return false
	}
	return cs.Mode == fact.ConsensusAuto
}

// mergeOne merges one pushed branch at its current tip; true when it wrote a
// merge commit.
func (m *consensusMerger) mergeOne(ctx context.Context, svc *store.Service, branch string) bool {
	tipStr, err := svc.Branches().HeadCommit(ctx, branch)
	if err != nil {
		return false // gone since the list; nothing to merge
	}
	tip := plumbing.NewHash(tipStr)
	m.mu.Lock()
	refusedTip, wasRefused := m.refused[branch]
	m.mu.Unlock()
	if wasRefused && refusedTip == tip {
		return false
	}
	if h := currentConsensusHooks().beforeMerge; h != nil {
		h(branch)
	}
	m.mu.Lock()
	m.stats.Attempts++
	m.mu.Unlock()

	res, err := svc.MergeConsensus(ctx, branch, m.agentBranch, tip)
	var conflict *store.MergeConflictError
	switch {
	case err == nil:
		m.mu.Lock()
		delete(m.refused, branch)
		if res.Mode == store.ModeMerge {
			m.stats.Merges++
		}
		m.mu.Unlock()
		if res.Mode != store.ModeMerge {
			return false
		}
		log.Info().Str("repo", m.repo).Str("branch", branch).Str("into", m.agentBranch).
			Str("merge_commit", shortHash(res.NewTip)).Msg("consensus: auto-merged a pushed branch")
		return true
	case errors.Is(err, store.ErrBranchMoved):
		return false // the push that moved it kicked us again
	case errors.As(err, &conflict):
		m.refuse(branch, tip, fmt.Sprintf(
			"consensus: auto-merge of %s at %s into %s refused: conflicting paths %s; left for a human merge (Pushed branches)",
			branch, shortHash(tipStr), m.agentBranch, strings.Join(conflict.Paths, ", ")))
	case errors.Is(err, store.ErrUnrelatedHistories), errors.Is(err, store.ErrNoSigner):
		m.refuse(branch, tip, fmt.Sprintf(
			"consensus: auto-merge of %s at %s into %s refused: %v; left for a human", branch, shortHash(tipStr), m.agentBranch, err))
	default:
		// Transient (a store swap, an I/O error): not remembered, so the
		// next kick retries; warned once per (branch, tip, error).
		m.warnOnce("error:"+branch+"@"+tipStr+":"+err.Error(), fmt.Sprintf(
			"consensus: auto-merge of %s at %s failed: %v; retried at the next kick", branch, shortHash(tipStr), err))
	}
	return false
}

func (m *consensusMerger) refuse(branch string, tip plumbing.Hash, msg string) {
	m.mu.Lock()
	m.refused[branch] = tip
	m.stats.Refused++
	m.mu.Unlock()
	m.warnOnce("refused:"+branch+"@"+tip.String(), msg)
}
