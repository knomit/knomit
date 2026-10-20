package store

import (
	"knomit/internal/store/vocab"
)

// This file is the parent's seam to the vocab child (internal/store/vocab):
// motif alias resolution and motif definitions (MotifIndex), and the
// title-embedding axis with its shortlist, both review-time derived state.
// Both hold a back-reference to the ONE *vcs.Repo the parent's repoHandler
// embeds (store.Open passes rh.Repo), so they reach UP to the git layer and
// never sideways; the child calls no method declared on repoHandler.
//
// The axis' own names (its type alias, its compile-time guard and the
// shortlist types whose names carry the axis' vocabulary) are re-exported in
// interfaces.go instead, next to the interface they implement:
// internal/synthesize TestConformance_ReviewPipelineOnly lets only
// interfaces.go, service.go and vocab/abstraction.go name them, and this file
// is not on that list. Every other public name the moved files declared is
// re-exported below, so the public surface of package store does not move.

// motifIndex keeps the parent's spelling for the Service field.
type motifIndex = vocab.MotifIndex

// Types.
type (
	AliasRow              = vocab.AliasRow
	DefinitionStamp       = vocab.DefinitionStamp
	DefinitionTarget      = vocab.DefinitionTarget
	LiveFactTitle         = vocab.LiveFactTitle
	MotifCluster          = vocab.MotifCluster
	MotifDefinitionStatus = vocab.MotifDefinitionStatus
	MotifVocabularyHealth = vocab.MotifVocabularyHealth
	TitleNeighbour        = vocab.TitleNeighbour
	TitleTarget           = vocab.TitleTarget
	TitleVector           = vocab.TitleVector
)

// Constants.
const MatchTitleKNN = vocab.MatchTitleKNN

// FactIDPairKey is vocab.FactIDPairKey.
func FactIDPairKey(a, b int64) string { return vocab.FactIDPairKey(a, b) }
