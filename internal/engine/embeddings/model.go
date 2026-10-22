// Package embeddings turns text into vectors via in-process ONNX inference.
// Models are described by a Model descriptor and selected from a built-in
// registry, which lives in the cgo-free internal/domain/modelparams
// (registry.go) so that knowing about a model never requires linking one.
package embeddings

import (
	"knomit/internal/domain/modelparams"
)

// Model is params.Model: the descriptor of an embedding model. An alias, so
// the inference code and every existing caller read exactly as before the
// registry moved to params.
type Model = modelparams.Model

// Pooling is params.Pooling: how a model's token-level output is reduced to
// one vector.
type Pooling = modelparams.Pooling

// The pooling modes, re-exported from params.
const (
	PoolNone      = modelparams.PoolNone
	PoolMean      = modelparams.PoolMean
	PoolCLS       = modelparams.PoolCLS
	PoolLastToken = modelparams.PoolLastToken
)

// Lookup returns the descriptor for id, or an error listing valid ids
// (params.Lookup).
func Lookup(id string) (Model, error) { return modelparams.Lookup(id) }

// IDs returns the sorted set of registered model ids (params.IDs).
func IDs() []string { return modelparams.IDs() }

// DefaultModelID is re-exported from internal/domain/modelparams, where it lives
// so that internal/foundation/config can reference it without importing this cgo-carrying
// package. Prefer params.DefaultModelID in new code.
const DefaultModelID = modelparams.DefaultModelID
