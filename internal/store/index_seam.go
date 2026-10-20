package store

import (
	"knomit/internal/store/index"
)

// This file is the parent's seam to the index child (internal/store/index):
// the search index (the IndexManager lifecycle, the write/rebuild/graph-write
// path, the vec tables) and the four read-side query services carved out of
// it (FactQuery, GraphStore, HistoryQuery, MethodologyMatcher) plus the
// stateless Facade that composes them into the SearchIndex composite.
//
// Every one of those holds a back-reference to the ONE *vcs.Repo the
// parent's repoHandler embeds (newService passes rh.Repo), so the sub-indices
// still reach UP to the git layer and never sideways through a sibling; the
// child calls no method declared on the parent's repoHandler, facts.Repo or
// merge.Repo (it imports facts only for the FactRecord / FactWithBody types
// and ParseFactRecord).
//
// The aliases below keep the parent's own spelling (searchIndex, factQuery,
// ...) for the Service fields, the interface guards and the white-box tests,
// and re-export every public name the moved files declared, so the public
// surface of package store does not move.
type (
	searchIndex        = index.Index
	factQuery          = index.FactQuery
	graphStore         = index.GraphStore
	historyQuery       = index.HistoryQuery
	methodologyMatcher = index.MethodologyMatcher
	searchFacade       = index.Facade
)

// Types.
type (
	ActivityResult     = index.ActivityResult
	ChangedFile        = index.ChangedFile
	CommitAuthor       = index.CommitAuthor
	CommitDetailResult = index.CommitDetailResult
	ExplainResult      = index.ExplainResult
	FactRevision       = index.FactRevision
	FactRow            = index.FactRow
	FactsIter          = index.FactsIter
	FileCounts         = index.FileCounts
	Highlight          = index.Highlight
	LogEntry           = index.LogEntry
	LogEntryWithTags   = index.LogEntryWithTags
	MethodologyMatch   = index.MethodologyMatch
	MotifMatchTier     = index.MotifMatchTier
	PathHistoryCursor  = index.PathHistoryCursor
	RecentFactEntry    = index.RecentFactEntry
	RefSummary         = index.RefSummary
	SearchDiag         = index.SearchDiag
	SearchOptions      = index.SearchOptions
	SearchResult       = index.SearchResult
	SimilarityGraph    = index.SimilarityGraph
	StatsResult        = index.StatsResult
	SubjectLabelDF     = index.SubjectLabelDF
)

// Constants.
const (
	GraphSchemaVersion = index.GraphSchemaVersion
	MaxHighlights      = index.MaxHighlights

	AxisImpact     = index.AxisImpact
	AxisConfidence = index.AxisConfidence
	AxisRecent     = index.AxisRecent

	NodeFact         = index.NodeFact
	NodeEntity       = index.NodeEntity
	NodeDomain       = index.NodeDomain
	NodeOntologyNode = index.NodeOntologyNode

	EdgeTagged          = index.EdgeTagged
	EdgeInDomain        = index.EdgeInDomain
	EdgeUnder           = index.EdgeUnder
	EdgeDerivedFrom     = index.EdgeDerivedFrom
	EdgeSimilarTo       = index.EdgeSimilarTo
	EdgeDomainChildOf   = index.EdgeDomainChildOf
	EdgeOntologyChildOf = index.EdgeOntologyChildOf

	MotifMatchExact  = index.MotifMatchExact
	MotifMatchStem   = index.MotifMatchStem
	MotifMatchToken2 = index.MotifMatchToken2
	MotifMatchToken1 = index.MotifMatchToken1
	MotifMatchSoft   = index.MotifMatchSoft
)

// Variables: the SAME values, so errors.Is matches across the seam.
var (
	AllMotifMatchTiers = index.AllMotifMatchTiers
	ErrFactNotLive     = index.ErrFactNotLive
)

// AxisFromSeparation is index.AxisFromSeparation.
func AxisFromSeparation(topFacts, topEdges, obsFacts, obsEdges int) string {
	return index.AxisFromSeparation(topFacts, topEdges, obsFacts, obsEdges)
}

// CanonicalizeTag is index.CanonicalizeTag.
func CanonicalizeTag(s string) string { return index.CanonicalizeTag(s) }

// CutoffNotice is index.CutoffNotice.
func CutoffNotice(text bool, minSimilarity float64, diags ...SearchDiag) string {
	return index.CutoffNotice(text, minSimilarity, diags...)
}

// DomainTagMatches is index.DomainTagMatches.
func DomainTagMatches(factTag, queryTag string) bool {
	return index.DomainTagMatches(factTag, queryTag)
}

// EntityTagMatches is index.EntityTagMatches.
func EntityTagMatches(factEntity, queryEntity string) bool {
	return index.EntityTagMatches(factEntity, queryEntity)
}

// FormatMethodologySection is index.FormatMethodologySection.
func FormatMethodologySection(matches []MethodologyMatch) string {
	return index.FormatMethodologySection(matches)
}

// IsMethodologyMarker is index.IsMethodologyMarker.
func IsMethodologyMarker(s string) bool { return index.IsMethodologyMarker(s) }

// NewSimilarityGraph is index.NewSimilarityGraph.
func NewSimilarityGraph(pairs [][2]string) SimilarityGraph { return index.NewSimilarityGraph(pairs) }

// NormalizeAxis is index.NormalizeAxis.
func NormalizeAxis(requested, fallback string) string {
	return index.NormalizeAxis(requested, fallback)
}

// ValidateMinSimilarity is index.ValidateMinSimilarity.
func ValidateMinSimilarity(v float64) error { return index.ValidateMinSimilarity(v) }
