// internal/okf/doc.go

// Package okf maps a set of knomit facts to a conformant Open Knowledge
// Format (OKF) v0.1 bundle. It is pure: no git, no SQLite, no HTTP, no clock.
// The same inputs always produce byte-identical output — the determinism the
// export's caching and cross-machine reproducibility depend on.
package okf

// MapperVersion is part of the generation marker key. Bump it on ANY change
// that alters bundle bytes, so previously generated bundles self-invalidate.
//
// 2: per-directory index.md links its concept documents (was: title text only),
// with markdown link-label delimiters escaped.
const MapperVersion = 2

// OKFVersion is emitted in the bundle-root index.md frontmatter only.
const OKFVersion = "0.1"
