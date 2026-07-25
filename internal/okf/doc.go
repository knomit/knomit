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
// 5: authored ontology under kb/, derived views under views/ (so a knomit
// topic named "domains" or "entities" cannot collide with a generated one);
// concepts link their domain/entity hubs from a "Related" body section.
// 4: OKF v0.2 — structured `sources`, `generated`, `status`; domain and
// entity hub documents; okf_version 0.2.
// 3: all links are relative (GitHub resolves a leading "/" against the repo
// root, breaking absolute links once the bundle is published there); citations
// resolve kb/ fact edges to their bundle documents and http(s) refs to links.
const MapperVersion = 5

// OKFVersion is emitted in the bundle-root index.md frontmatter only.
const OKFVersion = "0.2"
