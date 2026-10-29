package materialize

import (
	"errors"
	"fmt"
)

// ErrInvalid wraps a request materialize refuses as malformed: an option out
// of range, or a path that cannot name a fact.
var ErrInvalid = errors.New("invalid materialize request")

// ErrNotFound wraps a request whose fact cannot be read: not at the anchor,
// or in a repo the binding does not mount.
var ErrNotFound = errors.New("fact not found")

// The walk's bounds. Depth and fact caps are API knobs, not properties of a
// corpus: the defaults are the user's rulings, the maxima mirror the
// existing read surfaces (knomit_explain's walk depth, knomit_query's
// max_results ceiling).
const (
	DefaultDepth    = 4
	MaxDepth        = 10
	DefaultMaxNodes = 200
	MaxNodesLimit   = 500
)

// Options are the caller's choices for one materialization.
type Options struct {
	// Depth is the longest shortest-distance from the root that is expanded;
	// 0 is the root alone. Frame ancestors are not bounded by it.
	Depth int `json:"depth"`
	// MaxNodes caps the number of facts written, the root and ancestors
	// included.
	MaxNodes int `json:"max_nodes"`
	// Unpin resolves every ref to its target's latest version at the anchor
	// instead of the version the referrer cited.
	Unpin bool `json:"unpinned"`
	// CrossKB follows kb://<other>/… refs into any repo registered on the
	// server.
	CrossKB bool `json:"cross_kb"`
}

// DefaultOptions is what a caller gets for every option it leaves out.
func DefaultOptions() Options {
	return Options{Depth: DefaultDepth, MaxNodes: DefaultMaxNodes}
}

// Validate refuses an option outside its range. The message names the
// option and its range, so a caller can correct itself.
func (o Options) Validate() error {
	if o.Depth < 0 || o.Depth > MaxDepth {
		return fmt.Errorf("%w: depth must be between 0 and %d, got %d", ErrInvalid, MaxDepth, o.Depth)
	}
	if o.MaxNodes < 1 || o.MaxNodes > MaxNodesLimit {
		return fmt.Errorf("%w: max_nodes must be between 1 and %d, got %d", ErrInvalid, MaxNodesLimit, o.MaxNodes)
	}
	return nil
}
