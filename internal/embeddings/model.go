// Package embeddings turns text into vectors via in-process ONNX inference.
// Models are described by a Model descriptor and selected from a built-in
// registry, which lives in the cgo-free internal/embeddings/params
// (registry.go) so that knowing about a model never requires linking one.
package embeddings

import (
	"knomit/internal/embeddings/params"
)

// Model is params.Model: the descriptor of an embedding model. An alias, so
// the inference code and every existing caller read exactly as before the
// registry moved to params.
type Model = params.Model

// Pooling is params.Pooling: how a model's token-level output is reduced to
// one vector.
type Pooling = params.Pooling

// The pooling modes, re-exported from params.
const (
	PoolNone      = params.PoolNone
	PoolMean      = params.PoolMean
	PoolCLS       = params.PoolCLS
	PoolLastToken = params.PoolLastToken
)

// Lookup returns the descriptor for id, or an error listing valid ids
// (params.Lookup).
func Lookup(id string) (Model, error) { return params.Lookup(id) }

// IDs returns the sorted set of registered model ids (params.IDs).
func IDs() []string { return params.IDs() }

// DefaultModelID is re-exported from internal/embeddings/params, where it lives
// so that internal/config can reference it without importing this cgo-carrying
// package. Prefer params.DefaultModelID in new code.
const DefaultModelID = params.DefaultModelID
