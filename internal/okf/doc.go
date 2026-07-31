// internal/okf/doc.go

// Package okf maps a set of knomit facts to a conformant Open Knowledge
// Format (OKF) v0.2 bundle. It is pure: no git, no SQLite, no HTTP, no clock.
// The same inputs always produce byte-identical output — the determinism the
// export's caching and cross-machine reproducibility depend on.
package okf

// OKFVersion is emitted in the bundle-root index.md frontmatter only.
const OKFVersion = "0.2"
