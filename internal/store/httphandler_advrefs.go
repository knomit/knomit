package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/rs/zerolog/log"

	"knomit/internal/platform/version"
)

// capKnomitRepoID advertises the knowledge base this store holds — the 40-hex
// ROOT COMMIT of the consensus branch, which is a knowledge base's identity
// (kb/decisions/fact/src-ref-repo-id-commit-blob) and is identical in every
// copy of it.
//
// It exists so a subscriber can learn, from the ref advertisement alone,
// whether it ALREADY holds this knowledge base under another name — a refusal
// that used to arrive only after the whole clone (see
// kb/invariants/repos/one-local-copy-per-knowledge-base). Advertising it is
// safe with every client: git and go-git both ignore capabilities they do not
// know, and neither echoes one back.
//
// It is a claim the remote makes about ITSELF, and it is trusted only to
// REFUSE. Nothing records identity from it: the authoritative root commit is
// always the one computed on the local store after the clone.
const capKnomitRepoID = capability.Capability("knomit-repo-id")

// rootCommitCache memoises the consensus branch's root commit, keyed by the
// branch TIP it was computed from.
//
// CLASSIFICATION (MN13): not a corpus property — it stores one value the
// repository itself determines, and derives nothing. The key is the tip rather
// than the branch name because a root commit cannot change while the tip
// stands still: history is append-only in front of it, so any advance of the
// tip leaves the same root, and the only thing that can install a DIFFERENT
// root is a wholesale store replacement, which moves the tip too. Keying on
// the tip therefore makes a stale answer unrepresentable rather than unlikely,
// at the cost of one first-parent walk after each push.
type rootCommitCache struct {
	mu   sync.Mutex
	tip  plumbing.Hash
	root string
}

// rootFor returns the root commit reachable from tip, computing it at most
// once per distinct tip. A failure is NOT cached: it is retried on the next
// advertisement, and reported as "" so the caller can advertise nothing rather
// than a wrong identity.
func (c *rootCommitCache) rootFor(rh *repoHandler, branch string, tip plumbing.Hash) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.root != "" && c.tip == tip {
		return c.root
	}
	root, err := rh.rootCommit(context.Background(), branch)
	if err != nil {
		// Advertising no identity is the honest answer, and it costs only the
		// early refusal: the post-clone check (layer 3) is authoritative and
		// still runs.
		log.Warn().Err(err).Str("branch", branch).
			Msg("git advertise: root commit unresolved; knomit-repo-id not advertised")
		return ""
	}
	c.tip, c.root = tip, root
	return root
}

// errUpstreamMissing is returned when refs/heads/<upstream> does not exist.
// The repo is unservable: there is no consensus branch to put HEAD on, and a
// clone that "succeeds" against it comes away with nothing.
var errUpstreamMissing = errors.New("knomit: consensus branch does not exist in this store")

// buildAdvRefs is the served VIEW of the store's refs: HEAD is a symref to
// refs/heads/<upstream>; only refs/heads/<upstream> and refs/heads/agent/*
// are listed. refs/remotes/* and refs/knomit/* describe OTHER repos or private
// bookkeeping — advertising them told a subscriber to follow the serving
// instance's own agent branch, or the state of a third repo it has no
// relationship with.
//
// The returned set holds every advertised tip; upload-pack refuses wants
// outside it, which is git's own uploadpack.allowAnySHA1InWant=false default
// and what stops a hidden ref from being fetchable by hash.
func buildAdvRefs(rh *repoHandler, upstream string, roots *rootCommitCache) (*packp.AdvRefs, map[plumbing.Hash]struct{}, error) {
	ar := packp.NewAdvRefs()
	tips := map[plumbing.Hash]struct{}{}

	caps := ar.Capabilities
	_ = caps.Set(capability.Agent, "knomit/"+version.Version)
	_ = caps.Set(capability.OFSDelta)
	// shallow ONLY. deepen-since/deepen-not/deepen-relative are deliberately
	// not advertised, so no client ever sends them and the pack builder never
	// has to answer a request shape it does not implement.
	_ = caps.Set(capability.Shallow)
	// side-band-64k so a fetch can carry human-readable progress alongside the
	// pack. Advertising it is what lets a client ASK for it; a client that does
	// not ask gets exactly the bytes it got before (httphandler.go).
	_ = caps.Set(capability.Sideband64k)

	upstreamRef := plumbing.NewBranchReferenceName(upstream)
	ref, err := rh.gits.Reference(upstreamRef)
	if err != nil {
		// REFUSE, rather than serve a headless advertisement. Omitting HEAD
		// and logging at warn made `git clone` print
		// "warning: remote HEAD refers to nonexistent ref" and then exit 0
		// with an empty working tree — a subscriber would take that repo as
		// valid and hold nothing. A protocol error is the only outcome the
		// client cannot mistake for success.
		//
		// The reachable case is a configured Remote.Branch naming a branch
		// this store does not have (upstream "master", store holds "main").
		// EnsureLocalUpstream cannot repair it — it bootstraps only from
		// refs/remotes/origin/<upstream>, equally absent — and should not
		// try: inventing the branch would be guessing at consensus.
		log.Error().Str("upstream", upstream).
			Msg("git advertise: consensus branch missing; refusing to serve this repo")
		return nil, nil, fmt.Errorf("%w: %q", errUpstreamMissing, upstream)
	}
	h := ref.Hash()
	ar.Head = &h
	ar.References[upstreamRef.String()] = h
	tips[h] = struct{}{}
	_ = caps.Set(capability.SymRef, "HEAD:"+upstreamRef.String())
	if roots != nil {
		if root := roots.rootFor(rh, upstream, h); root != "" {
			_ = caps.Set(capKnomitRepoID, root)
		}
	}

	iter, err := rh.gits.IterReferences()
	if err != nil {
		return nil, nil, fmt.Errorf("advrefs: iter references: %w", err)
	}
	defer iter.Close()
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if ref.Type() != plumbing.HashReference || !strings.HasPrefix(name, "refs/heads/agent/") {
			return nil
		}
		ar.References[name] = ref.Hash()
		tips[ref.Hash()] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("advrefs: walk: %w", err)
	}
	return ar, tips, nil
}
