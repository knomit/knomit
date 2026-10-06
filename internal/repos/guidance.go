package repos

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// ConsensusGuidance is the repository's guidance (F23) as its consensus branch
// says it at ONE commit: the ontology at the tip, and the text of every
// guidance file it names, read at that same commit.
//
// It is the only source of guidance text. Nothing here reads the agent
// branch, an experiment, or the ontology the repo opened with: an unmerged
// edit must not steer prompts, and guidance is trusted exactly as far as
// skills and recipes are (whoever can change the consensus branch).
type ConsensusGuidance struct {
	// Branch is the consensus branch, as UpstreamBranch() names it.
	Branch string
	// Commit is the full hash of the tip everything was read at.
	Commit string
	// Ontology is the ontology at Commit, parsed as the open path parses
	// (warnings tolerated).
	Ontology *fact.Ontology
	// texts maps a guidance path as written in the ontology (guidance/x.md)
	// to its text. A path that is missing or unusable at Commit is absent.
	texts map[string]string
}

// Text returns the text of guidance path p, false when it was skipped.
func (g *ConsensusGuidance) Text(p string) (string, bool) {
	if g == nil {
		return "", false
	}
	t, ok := g.texts[p]
	return t, ok
}

// ShortCommit is the commit's first 7 hex digits, for a prompt header.
func (g *ConsensusGuidance) ShortCommit() string {
	if len(g.Commit) > 7 {
		return g.Commit[:7]
	}
	return g.Commit
}

// NewConsensusGuidanceForTest builds a snapshot without a store, for the
// renderers' unit tests in other packages.
func NewConsensusGuidanceForTest(branch, commit string, o *fact.Ontology, texts map[string]string) *ConsensusGuidance {
	return &ConsensusGuidance{Branch: branch, Commit: commit, Ontology: o, texts: maps.Clone(texts)}
}

// guidanceCache holds the snapshot of the latest consensus tip read. It is
// rebuilt when the tip moves, so an edit that reaches the consensus branch
// takes effect on the next work item with no restart; building under mu is
// what makes each warning fire once per (path, commit).
type guidanceCache struct {
	mu     sync.Mutex
	commit plumbing.Hash
	built  bool // snap is the complete answer for commit (may be nil)
	snap   *ConsensusGuidance
	// warned holds the warnings already logged for commit. Cleared when the
	// tip moves.
	warned map[string]bool
	// warnedNoBranch: the "no consensus branch yet" warning was logged.
	warnedNoBranch bool
}

// guidanceWarnHook, when set, is told every guidance warning logged (tests).
var guidanceWarnHook func(msg string)

// SetGuidanceWarnHookForTest makes h see every guidance warning logged, and
// returns the function that removes it. Tests only; never in parallel.
func SetGuidanceWarnHookForTest(h func(msg string)) (restore func()) {
	prev := guidanceWarnHook
	guidanceWarnHook = h
	return func() { guidanceWarnHook = prev }
}

// ConsensusGuidance reads the guidance at the tip of ri's consensus branch.
// nil means none: no consensus branch yet, no ontology there, one that does
// not parse, or the store is unavailable — each warned once, never a fallback
// to another branch.
func (ri *RepoInstance) ConsensusGuidance(ctx context.Context) *ConsensusGuidance {
	svc, release, err := ri.Acquire()
	if err != nil {
		log.Debug().Err(err).Str("repo", ri.Name()).Msg("guidance: store unavailable; no guidance")
		return nil
	}
	defer release()
	return ri.guidance.at(ctx, ri.Name(), svc)
}

func (c *guidanceCache) at(ctx context.Context, repo string, svc *store.Service) *ConsensusGuidance {
	branch := svc.UpstreamBranch()
	tr := svc.Triggers()
	tip, err := tr.UpstreamTip(ctx, branch)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		// Not cached: a read error says nothing about the commit.
		c.warn(repo, "tip:"+err.Error(), fmt.Sprintf("guidance: read the consensus branch %s: %v; no guidance", branch, err))
		return nil
	}
	if tip.IsZero() {
		if !c.warnedNoBranch {
			c.warnedNoBranch = true
			c.log(repo, fmt.Sprintf("guidance: the consensus branch %s does not exist yet; no guidance", branch))
		}
		return nil
	}
	if c.built && c.commit == tip {
		return c.snap
	}
	if c.commit != tip {
		c.commit, c.warned, c.built, c.snap = tip, map[string]bool{}, false, nil
	}
	snap, complete := c.build(ctx, repo, tr, branch, tip)
	if complete {
		c.built, c.snap = true, snap
	}
	return snap
}

// build reads the ontology and every guidance file at tip. complete is false
// when a read failed for a reason other than the file's own state (a store
// error), so the next call retries instead of keeping the gap for this commit.
func (c *guidanceCache) build(ctx context.Context, repo string, tr store.TriggerIndex, branch string, tip plumbing.Hash) (*ConsensusGuidance, bool) {
	at := branch + "@" + tip.String()[:7]
	_, blob, data, err := tr.OntologyAtCommit(ctx, tip)
	switch {
	case errors.Is(err, store.ErrNoOntologyAtCommit):
		c.warn(repo, "ontology", fmt.Sprintf("guidance: no ontology at %s; no guidance", at))
		return nil, true
	case err != nil:
		c.warn(repo, "ontology", fmt.Sprintf("guidance: read the ontology at %s: %v; no guidance", at, err))
		return nil, false
	}
	o, err := fact.ParseOntology(data)
	if err != nil {
		c.warn(repo, "ontology", fmt.Sprintf("guidance: the ontology at %s (blob %s) does not parse (%v); no guidance", at, blob, err))
		return nil, true
	}
	g := &ConsensusGuidance{Branch: branch, Commit: tip.String(), Ontology: o, texts: map[string]string{}}
	complete := true
	decl := o.GuidanceDeclarations()
	for _, p := range slices.Sorted(maps.Keys(decl)) {
		_, body, err := tr.GuidanceAt(ctx, tip, fact.GuidanceFile(p))
		if err == nil {
			g.texts[p] = string(body)
			continue
		}
		why := err.Error()
		if errors.Is(err, store.ErrNoGuidanceAtCommit) {
			why = "the file does not exist there"
		} else if !errors.Is(err, store.ErrGuidanceUnusable) {
			complete = false
		}
		c.warn(repo, "path:"+p, fmt.Sprintf("guidance: %s (declared by %s) skipped at %s: %s",
			fact.GuidanceFile(p), strings.Join(decl[p], ", "), at, why))
	}
	return g, complete
}

// warn logs msg once per key for the current commit. Callers hold c.mu.
func (c *guidanceCache) warn(repo, key, msg string) {
	if c.warned == nil {
		c.warned = map[string]bool{}
	}
	if c.warned[key] {
		return
	}
	c.warned[key] = true
	c.log(repo, msg)
}

func (c *guidanceCache) log(repo, msg string) {
	log.Warn().Str("repo", repo).Msg(msg)
	if h := guidanceWarnHook; h != nil {
		h(msg)
	}
}
