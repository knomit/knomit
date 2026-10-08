package synthesize

import (
	"knomit/internal/refs"
	"knomit/internal/store"
)

// writeGate is the ONE constructor of the ref gate every synthesize write goes
// through (reflect, distill, prune merge, dedup merge, discovery, reinforce).
// It checks a newly added ref to a fact with idx and a newly added ref to a
// .knomit/ file with files, both at the tip of branch — the branch the write
// lands on, the session's own (invariants/synthesize/session-branch-binding).
//
// files is the FactIndex the caller writes through: Pipeline.storeIndices
// resolves it in the same read lock as idx, so the two resolvers never come
// from two different Services. A nil files (fixtures with no git store) leaves
// the gate without a file resolver, which REFUSES a new .knomit/ ref rather
// than admitting it.
//
// Only reflect, distill and the prune merge can meet a NEW .knomit/ ref (their
// refs come from the model). Dedup, discovery and reinforce add only fact paths
// or carry refs as prior; they use this constructor anyway so that no gate in
// this package is built without the file resolver.
// TestSynthesize_OneRefGateConstructor keeps it that way.
func writeGate(localRepoID string, idx store.FactQuery, files store.SystemFileIndex, branch string) refs.Gate {
	g := refs.New(localRepoID, refs.FromFactQuery(idx, branch))
	if files != nil {
		g = g.WithFiles(refs.FromSystemFiles(files, branch))
	}
	return g
}
