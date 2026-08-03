package web

import (
	"context"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// branchesListerFunc enumerates a repo's branches. It is a named type rather
// than an inline func literal so the signature has exactly one definition to
// change when the shape evolves (as it did when ctx threading landed).
type branchesListerFunc func(ctx context.Context, ri *repos.RepoInstance) ([]store.Branch, error)

// branchRootReaderFunc reads the head + index watermark for one branch. Named
// for the same reason as branchesListerFunc.
type branchRootReaderFunc func(ctx context.Context, ri *repos.RepoInstance, branch string) (branchRootInfo, error)

// storeProviders bundles every injectable data-access seam the API router
// wires into handlers. Before this existed the Server carried fourteen
// separate unexported fields and NewAPIRouter carried fourteen near-identical
// nil-check-then-default blocks interleaved with route registrations — the
// wiring was scattered through 300 lines of routing and two of the locals
// collided on name (sp/sp2).
//
// The zero value means "all defaults": callers materialize the production
// implementations with withDefaults(). Tests set only the members they stub.
//
// This is a construction-time wiring artifact, scoped to Server and
// NewAPIRouter — deliberately NOT a runtime service locator. Handlers keep
// their narrow constructor signatures so each one's true dependency set stays
// visible at its registration site; passing the bundle around would make
// every handler nominally depend on all fourteen seams.
type storeProviders struct {
	branchesLister   branchesListerFunc
	branchRootReader branchRootReaderFunc
	factReader       FactReader
	factWriter       FactWriter
	topicLister      TopicLister
	search           searchProvider
	commits          commitsProvider
	factSub          factSubProvider
	stats            statsProvider
	domains          domainsProvider
	completions      completionsProvider
	factsCollection  factsCollectionProvider
	activity         activityProvider
	origin           originProvider
	repoNamer        RepoNamer
}

// withDefaults returns a copy with every nil member replaced by its
// production implementation.
//
// The value receiver is load-bearing: the Server's stored bundle is never
// mutated, so a test's sparse overrides stay sparse and NewAPIRouter can be
// called twice on the same Server with identical results.
//
// m is the mount table, needed only by repoNamer — the one seam whose
// production implementation is a method on live state rather than a stateless
// default. A nil m leaves repoNamer nil, which BuildRefViews reads as "no repo
// id can be named" and renders ids unchanged.
func (p storeProviders) withDefaults(m *repos.Manager) storeProviders {
	if p.branchesLister == nil {
		p.branchesLister = defaultBranchesLister
	}
	if p.branchRootReader == nil {
		p.branchRootReader = defaultBranchRootReader
	}
	if p.factReader == nil {
		p.factReader = defaultFactReader{}
	}
	if p.factWriter == nil {
		p.factWriter = defaultFactWriter{}
	}
	if p.topicLister == nil {
		p.topicLister = defaultTopicLister{}
	}
	if p.search == nil {
		p.search = defaultSearchProvider{}
	}
	if p.commits == nil {
		p.commits = defaultCommitsProvider{}
	}
	if p.factSub == nil {
		p.factSub = defaultFactSubProvider{}
	}
	if p.stats == nil {
		p.stats = defaultStatsProvider{}
	}
	if p.domains == nil {
		p.domains = defaultDomainsProvider{}
	}
	if p.completions == nil {
		p.completions = defaultCompletionsProvider{}
	}
	if p.factsCollection == nil {
		p.factsCollection = defaultFactsCollectionProvider{}
	}
	if p.activity == nil {
		p.activity = defaultActivityProvider{}
	}
	if p.origin == nil {
		p.origin = defaultOriginProvider{}
	}
	if p.repoNamer == nil && m != nil {
		p.repoNamer = m.NameByID
	}
	return p
}
