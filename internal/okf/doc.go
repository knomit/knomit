// internal/okf/doc.go

// Package okf maps a set of knomit facts to a conformant Open Knowledge
// Format (OKF) v0.2 bundle. It is pure: no git, no SQLite, no HTTP, no clock.
// The same inputs always produce byte-identical output — the determinism the
// export's caching and cross-machine reproducibility depend on.
package okf

// MapperVersion is part of the generation marker key. Bump it on ANY change
// that alters bundle bytes, so previously generated bundles self-invalidate.
//
// 13: withdrawn knowledge is published as views/retired.md — one index naming
// every retracted or superseded fact, with superseded entries linking their
// replacement forward. Retired facts get NO concept document (a consumer may
// ignore `status`, so an ingestible one would re-assert a disavowed claim), and
// live documents are unchanged.
// 12: incoming edges — each concept lists the facts that cite IT in a
// "Cited by" section (between Citations and History), making the derivation
// graph traversable in both directions; plus a methodology digest under
// views/, completing the fabricated-type views beside synthesis and hypotheses.
// 11: History revisions are ordered chronologically on same-second ties (by
// caller order, not operation/digest), and revisions that changed nothing we
// track are dropped instead of rendering as a "revised" no-op line.
// 10: per-fact History section showing how a belief evolved (operation +
// semantic delta per revision).
// 9: citations are labelled with the cited fact.s title instead of its raw
// path, and that title reaches the v0.2 sources entry.
// 8: digests grouped by month then day (a flat day list is unusable at
// scale); lowercase digest labels; unknown topics keep their name verbatim.
// 7: alphabetical jump index on long hub indexes; single-file synthesis and
// hypothesis digests grouped by day.
// 6: index prose and entry descriptions from the authored ontology
// (domains/ontology.yaml at the source commit).
// 5: authored ontology under kb/, derived views under views/ (so a knomit
// topic named "domains" or "entities" cannot collide with a generated one);
// concepts link their domain/entity hubs from a "Related" body section.
// 4: OKF v0.2 — structured `sources`, `generated`, `status`; domain and
// entity hub documents; okf_version 0.2.
// 3: all links are relative (GitHub resolves a leading "/" against the repo
// root, breaking absolute links once the bundle is published there); citations
// resolve kb/ fact edges to their bundle documents and http(s) refs to links.
// 2: per-directory index.md links its concept documents (was: title text only),
// with markdown link-label delimiters escaped.
const MapperVersion = 13

// OKFVersion is emitted in the bundle-root index.md frontmatter only.
const OKFVersion = "0.2"
