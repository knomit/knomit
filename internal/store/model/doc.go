// Package model holds the plain data the store's children define and callers
// outside the store use: row and request shapes (Branch, Experiment,
// ChangesQuery, PipelineSession, ...), the constants and sentinel errors that
// go with them, and the pure functions over them (trailer and cursor codecs,
// the consensus-branch choice rule, cosine similarity, credential encryption).
//
// It is a LEAF: it imports nothing from the store or its children, so vcs,
// sessions, sqlite and every caller outside the store can import it without a
// cycle, and a sentinel here is ONE value however it is reached (errors.Is
// needs no re-export). TestStoreModel_IsALeaf pins the rule.
//
// Anything that needs a repository, a database or a child's package state
// does not belong here; it stays in its child.
package model
