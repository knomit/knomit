package store

import "knomit/internal/engine/store/vocab"

// pairKey is the vocab child's unexported pair identity, under the name this
// package's white-box tests used before the move (a *ForTest seam that panics
// outside a test binary).
var pairKey = vocab.PairKeyForTest
