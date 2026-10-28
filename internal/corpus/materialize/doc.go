// Package materialize is the read-side renderer of ONE fact's reference
// closure: the fact plus the facts it stands on, in a fixed reading order, as
// of one anchor commit. It serves two audiences from the same ordered node
// list: an agent document (mechanical markdown, no model call) and a human
// page stream (the same nodes as data, plus formatting instructions for the
// caller's own model).
//
// Scope, stated because "anything ref-related goes here" is the easy
// misreading: materialize is NOT ref resolution at write time (that is
// internal/corpus/refs) and NOT lens fusion (internal/corpus/federate). It
// only READS: a ref's version comes from the DERIVED_FROM edge the write path
// already recorded, or, with Options.Unpin, from the target's latest version.
//
// The walk (Plan):
//   - Content is the same for every fact: a breadth-first pass to
//     Options.Depth decides the included set (shortest distance from the
//     root, cut at Options.MaxNodes in BFS order), and a pre-order depth-first
//     pass in the order each fact's refs are written decides the reading
//     order. A path is written once; a later citation is a back-reference.
//   - A frame (fact.Frame) differs only in ORDER: its ancestors come first,
//     topologically, then the frame, then the ordinary walk. A parent is any
//     ref whose target is a frame. Ancestors are included whatever their
//     distance and are never expanded.
//   - Pinned by default: every hop reads the version the referrer cited (its
//     edge's target commit), so the output is a pure function of (root,
//     anchor, options). Unpin reads every target's latest version at the
//     anchor and silently drops a target that is gone there. Cross-KB refs
//     are followed only with Options.CrossKB, into any repo registered on the
//     server, at that repo's latest version.
//
// context.status is ignored entirely: knomit never acts on a context value.
package materialize
