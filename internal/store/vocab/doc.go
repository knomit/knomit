// Package vocab is the store's vocabulary cluster: motif alias resolution and
// motif definitions (MotifIndex, motif_alias.go and motif_definition.go), and
// the title-embedding axis with the shortlist built on it (abstraction.go),
// both review-time derived state. It sits above sqlite and vcs in the store's
// child order and reads a store only through *vcs.Repo; the facade reaches up
// to it, never the reverse.
//
// The constructors live beside their types (abstraction.go, motif_alias.go):
// internal/synthesize TestConformance_ReviewPipelineOnly lets only
// vocab/abstraction.go name the axis, so no other file here may.
package vocab
