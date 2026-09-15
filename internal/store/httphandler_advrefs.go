package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/rs/zerolog/log"

	"knomit/internal/platform/version"
)

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
func buildAdvRefs(rh *repoHandler, upstream string) (*packp.AdvRefs, map[plumbing.Hash]struct{}, error) {
	ar := packp.NewAdvRefs()
	tips := map[plumbing.Hash]struct{}{}

	caps := ar.Capabilities
	_ = caps.Set(capability.Agent, "knomit/"+version.Version)
	_ = caps.Set(capability.OFSDelta)
	// shallow ONLY. deepen-since/deepen-not/deepen-relative are deliberately
	// not advertised, so no client ever sends them and the pack builder never
	// has to answer a request shape it does not implement.
	_ = caps.Set(capability.Shallow)

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
